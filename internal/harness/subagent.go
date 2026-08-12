package harness

import (
	"log"
	"time"

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
		// Only an agent gets a session. Claude Code backgrounds a shell command
		// through these same frames, so tracking every task promoted `sleep 2`
		// to a linked subagent session — 'agent-bhrfxpye5', display name
		// "Trigger on-demand discovery and watch for links", observed live.
		//
		// An unrecognized kind is logged rather than passed over quietly: it is
		// either a task kind we should be promoting and are not, or one we
		// should keep ignoring, and only the log makes that visible.
		if !msg.TaskTypeIsAgent(ev.System.TaskType) {
			if ev.System.TaskType != msg.TaskTypeLocalBash && !r.warned[ev.System.TaskType] {
				r.warned[ev.System.TaskType] = true
				log.Printf("[subagent] %s: not promoting task_type=%q to a session (task=%s, %q); if that is an agent kind, add it to msg.TaskTypeIsAgent",
					r.parentID, ev.System.TaskType, ev.System.TaskID, ev.System.Description)
			}
			return
		}
		if existing, ok := r.byToolUse[ev.System.ToolUseID]; ok {
			// Claude Code repeats task_started for a task it has already
			// announced. The second one must carry the same session id as the
			// first, not an empty field.
			ev.System.SubagentSessionID = existing.bridgeID
			return
		}
		task := &subagentTask{taskID: ev.System.TaskID, description: ev.System.Description}
		r.byToolUse[ev.System.ToolUseID] = task
		r.byTaskID[ev.System.TaskID] = task
		// Mint here, at the announcement, rather than when the subagent's first
		// frame arrives. This is the event that tells a client a subagent
		// exists, and the pump marshals it a few lines further on, so the id has
		// to exist by now or the client is left holding a harness-scoped task id
		// and nothing to follow.
		//
		// It also settles a task that dies before emitting a single frame. Under
		// the old lazy mint such a task never got a row, and settle and
		// settleUnfinished both skip a task with no row — so its terminal status
		// went nowhere and it later reappeared through discovery as an unlinked
		// orphan.
		ev.System.SubagentSessionID = r.ensureSession(ev.System.ToolUseID, task)

	case "task_updated", "task_notification":
		// The closing frames name their task too, so they can carry the link as
		// cheaply as the opening one. A client that shows the finish separately
		// from the spawn then has the same id on both rows.
		if task := r.byTaskID[ev.System.TaskID]; task != nil {
			ev.System.SubagentSessionID = task.bridgeID
		}
		r.settle(ev)
	}
}

// warnOnce logs one line per distinct subagent. A failure that recurs on every
// frame of the same task would otherwise fill the log with one line per frame.
func (r *subagentRouter) warnOnce(toolUseID, format string, args ...any) {
	if r.warned[toolUseID] {
		return
	}
	r.warned[toolUseID] = true
	log.Printf("[subagent] %s: "+format, append([]any{r.parentID}, args...)...)
}

// ensureSession returns the subagent's own bridge session id, minting it on
// first call. It returns "" when the session cannot be created, and every
// caller keeps the frame on the parent in that case rather than dropping it.
//
// observe calls this the moment task_started names the task; route calls it
// again for a task whose mint failed there, so a transient store error costs
// one event's link instead of stranding the subagent for the life of the
// process.
func (r *subagentRouter) ensureSession(toolUseID string, task *subagentTask) string {
	if task.bridgeID != "" {
		return task.bridgeID
	}
	parent, err := r.parentSession()
	if err != nil {
		r.warnOnce(toolUseID, "load parent session: %v", err)
		return ""
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
		r.warnOnce(toolUseID, "create session for task %s: %v", task.taskID, err)
		return ""
	}
	task.bridgeID = bridgeID
	if created {
		log.Printf("[subagent] %s spawned %s (task=%s, %q)", r.parentID, bridgeID, task.taskID, displayName)
	}
	return bridgeID
}

// settle closes out a subagent session when the parent reports its task
// reached a terminal status. Nothing else would ever settle it: the subagent's
// own frames carry no result event, so without this its session row would sit
// at "running" forever.
func (r *subagentRouter) settle(ev *msg.Event) {
	if ev.System.TaskID == "" {
		return
	}
	task := r.byTaskID[ev.System.TaskID]
	if task == nil || task.bridgeID == "" || task.settled {
		return
	}

	// The adapter normalizes the status onto TaskStatus. This used to
	// json.Unmarshal ev.Raw to recover it, because the canonical type could
	// not carry it — a second parser of Claude Code's wire format living in a
	// layer that should not know that format at all, and one that would have
	// gone quietly wrong the day CC renamed the field.
	if !msg.TaskStatusIsTerminal(ev.System.TaskStatus) {
		// in_progress, empty, and any status a harness adds later: not
		// terminal, so the subagent keeps running. Settling on an unknown
		// status would park a session that is still working.
		return
	}

	state := msg.SessionIdle
	if ev.System.TaskStatus != msg.TaskStatusCompleted {
		state = msg.SessionError
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
		r.warnOnce(ev.HarnessParentID, "frame with parent_tool_use_id=%s arrived before its task_started; routing to parent", ev.HarnessParentID)
		return r.parentID
	}
	// Normally observe already minted this at task_started; the call here
	// retries a mint that failed then.
	if bridgeID := r.ensureSession(ev.HarnessParentID, task); bridgeID != "" {
		return bridgeID
	}
	return r.parentID
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
//
// It emits a terminal task event on the parent as well as writing the subagent's
// row. Writing the row alone was not enough and read as a smaller bug than it
// was: the parent's stream is where a client learns that a task closed, so a
// silent row update left the spawn open forever on the parent's timeline and
// left the live status line showing a subagent that had been dead since the
// process died. One event settles all three.
//
// Measured on this host: 12 of 20 unfinished spawns in the 40 most recent
// sessions were followed by a fresh init, i.e. the process died with tasks in
// flight. This is the common case, not the rare one.
func (r *subagentRouter) settleUnfinished() {
	for toolUseID, task := range r.byToolUse {
		if task.settled {
			continue
		}
		task.settled = true

		// Cancelled, not failed: the task did not report a failure, it was
		// taken away mid-flight. This is the same canonical status the adapter
		// maps Claude Code's own "stopped"/"killed" onto.
		r.manager.broadcastDerived(r.parentID, &msg.Event{
			Type:      msg.EventSystem,
			Timestamp: time.Now(),
			System: &msg.SystemEvent{
				Subtype:           "task_updated",
				TaskID:            task.taskID,
				ToolUseID:         toolUseID,
				TaskStatus:        msg.TaskStatusCancelled,
				SubagentSessionID: task.bridgeID,
				Message:           "the harness process exited before this task reported a status",
			},
		})

		if task.bridgeID == "" {
			continue
		}
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
