package harness

import (
	"encoding/json"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// subagentPurpose tags sessions promoted out of a parent harness process, and
// is also the key folderForPurpose maps to the sidebar folder they land in.
const subagentPurpose = "subagent"

// subagentTask is one harness subagent, as announced by the parent's
// task_started narration and identified thereafter by the tool_use_id of the
// Task call that spawned it.
type subagentTask struct {
	taskID      string
	description string
	bridgeID    string // the subagent's own session, once minted
	settled     bool   // a terminal task_updated/task_notification has landed
}

// subagentRouter demuxes one harness process's event stream into per-subagent
// bridge sessions.
//
// Claude Code runs a Task subagent inside the parent's process and writes the
// subagent's frames onto the same stdout, stamped with the parent's
// session_id. The only field that separates them is parent_tool_use_id, which
// the adapter carries through as Event.HarnessParentID. Without this demux
// every subagent's work collapses onto the parent session and the only record
// that the subagent ever had a parent is the parent's own narration — which is
// how ~1000 subagent rows ended up with no link to what spawned them.
//
// The router mints one session per subagent, linked to the parent through
// manager_session_id, and re-points that subagent's frames at it. The parent's
// task_started / task_progress narration deliberately stays on the parent: the
// parent is the one narrating "I spawned a task".
//
// One router per harness process, owned and driven entirely by that process's
// readEvents goroutine, so it holds no locks of its own.
type subagentRouter struct {
	manager  *Manager
	parentID string

	parent    *store.Session // cached parent row; lineage fields are read from it
	byToolUse map[string]*subagentTask
	byTaskID  map[string]*subagentTask
	warned    map[string]bool
}

func newSubagentRouter(m *Manager, parentBridgeID string) *subagentRouter {
	return &subagentRouter{
		manager:   m,
		parentID:  parentBridgeID,
		byToolUse: make(map[string]*subagentTask),
		byTaskID:  make(map[string]*subagentTask),
		warned:    make(map[string]bool),
	}
}

// observe reads the parent's task narration for the facts the demux needs.
// Called for every event before routing, on the parent's stream only —
// task_started and friends never carry a parent_tool_use_id.
func (r *subagentRouter) observe(ev *msg.Event) {
	if ev.Type != msg.EventSystem || ev.System == nil {
		return
	}
	switch ev.System.Subtype {
	case "task_started":
		// task_started is the only frame that ties a tool_use_id to a task_id,
		// and the task_id is what builds the dedupe key shared with discovery.
		// Verified against a live capture: it always precedes the subagent's
		// own frames.
		if ev.System.ToolUseID == "" || ev.System.TaskID == "" {
			return
		}
		if _, ok := r.byToolUse[ev.System.ToolUseID]; ok {
			return
		}
		task := &subagentTask{taskID: ev.System.TaskID, description: ev.System.Description}
		r.byToolUse[ev.System.ToolUseID] = task
		r.byTaskID[ev.System.TaskID] = task

	case "task_updated", "task_notification":
		r.settle(ev)
	}
}

// settle closes out a subagent session when the parent reports its task
// reached a terminal status. Nothing else would ever settle it: the subagent's
// own frames carry no result event, so without this its session row would sit
// at "running" forever.
func (r *subagentRouter) settle(ev *msg.Event) {
	if ev.System.TaskID == "" || len(ev.Raw) == 0 {
		return
	}
	task := r.byTaskID[ev.System.TaskID]
	if task == nil || task.bridgeID == "" || task.settled {
		return
	}

	// Status lives in the raw frame: task_updated carries {"patch":{"status":…}}
	// and task_notification carries a top-level "status". Neither is surfaced on
	// the canonical SystemEvent, so read the frame the adapter preserved.
	var frame struct {
		Status string `json:"status"`
		Patch  struct {
			Status string `json:"status"`
		} `json:"patch"`
	}
	if err := json.Unmarshal(ev.Raw, &frame); err != nil {
		return
	}
	status := frame.Patch.Status
	if status == "" {
		status = frame.Status
	}

	var state msg.SessionState
	switch status {
	case "completed":
		state = msg.SessionIdle
	case "failed", "cancelled":
		state = msg.SessionError
	default:
		// in_progress and any status CC adds later: not terminal, leave it running.
		return
	}

	task.settled = true
	if err := r.manager.store.UpdateSessionState(task.bridgeID, string(state)); err != nil {
		log.Printf("[subagent] %s: settle to %s: %v", task.bridgeID, state, err)
	}
}

// route returns the bridge session id an event belongs to: the subagent's own
// session when the frame carries a parent_tool_use_id, the parent otherwise.
//
// It falls back to the parent — today's behaviour — whenever the subagent
// cannot be identified, so an unrecognized frame is still recorded somewhere
// rather than dropped. Each distinct unknown id is logged once; a steady
// stream of those means CC changed the ordering guarantee that task_started
// precedes the subagent's frames.
func (r *subagentRouter) route(ev *msg.Event) string {
	if ev.HarnessParentID == "" {
		return r.parentID
	}
	task := r.byToolUse[ev.HarnessParentID]
	if task == nil {
		if !r.warned[ev.HarnessParentID] {
			r.warned[ev.HarnessParentID] = true
			log.Printf("[subagent] %s: frame with parent_tool_use_id=%s arrived before its task_started; routing to parent", r.parentID, ev.HarnessParentID)
		}
		return r.parentID
	}
	if task.bridgeID != "" {
		return task.bridgeID
	}

	parent, err := r.parentSession()
	if err != nil {
		if !r.warned[ev.HarnessParentID] {
			r.warned[ev.HarnessParentID] = true
			log.Printf("[subagent] %s: load parent session: %v; routing to parent", r.parentID, err)
		}
		return r.parentID
	}

	// The dedupe key must match what the discovery scanner derives from the
	// rollout filename (subagents/agent-<task_id>.jsonl) so the live row and
	// the discovered one converge instead of doubling up.
	harnessSessionID := "agent-" + task.taskID
	displayName := task.description
	if displayName == "" {
		displayName = task.taskID
	}

	bridgeID, created, err := r.manager.store.EnsureSubagentSession(
		parent, harnessSessionID, displayName, r.manager.folderForPurpose(subagentPurpose))
	if err != nil {
		if !r.warned[ev.HarnessParentID] {
			r.warned[ev.HarnessParentID] = true
			log.Printf("[subagent] %s: create session for task %s: %v; routing to parent", r.parentID, task.taskID, err)
		}
		return r.parentID
	}
	task.bridgeID = bridgeID
	if created {
		log.Printf("[subagent] %s spawned %s (task=%s, %q)", r.parentID, bridgeID, task.taskID, displayName)
	}
	return bridgeID
}

// sessionIDs returns every subagent session this process promoted, so the
// caller can tear down their per-session state alongside the parent's.
func (r *subagentRouter) sessionIDs() []string {
	out := make([]string, 0, len(r.byToolUse))
	for _, task := range r.byToolUse {
		if task.bridgeID != "" {
			out = append(out, task.bridgeID)
		}
	}
	return out
}

// settleUnfinished closes out subagents whose host process exited before their
// task reported a terminal status — a kill, a crash, or the turn watchdog
// firing. Without it those rows stay at "running" with nothing left alive to
// ever move them, which is the same stuck-state class the F1 settle reconcile
// exists to prevent on parent sessions.
func (r *subagentRouter) settleUnfinished() {
	for _, task := range r.byToolUse {
		if task.bridgeID == "" || task.settled {
			continue
		}
		task.settled = true
		if err := r.manager.store.UpdateSessionState(task.bridgeID, string(msg.SessionError)); err != nil {
			log.Printf("[subagent] %s: settle abandoned subagent: %v", task.bridgeID, err)
		}
	}
}

// parentSession loads and caches the parent row. The subagent inherits its
// harness, instance, folder and tree position from it.
func (r *subagentRouter) parentSession() (*store.Session, error) {
	if r.parent != nil {
		return r.parent, nil
	}
	parent, err := r.manager.store.GetSession(r.parentID)
	if err != nil {
		return nil, err
	}
	r.parent = parent
	return parent, nil
}
