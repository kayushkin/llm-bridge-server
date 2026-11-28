package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// autoResumeWindow caps how recently a session must have been active (by
// LastActivityAt) for startup reconciliation to auto-restart its harness.
// Sessions older than this are left at idle for the user to resume manually.
// Tuned for Claude Code: turns regularly go quiet for tens of minutes (long
// thinking, user reading, etc.) — anything shorter drops still-active sessions
// across deploys.
const autoResumeWindow = 2 * time.Hour

// watchdogInterval is how often startWatchdog scans for `running` sessions
// whose harness process has died without emitting a terminal event (crash,
// OOM, harness binary panic). Each tick reconciles via autoResume.
const watchdogInterval = 60 * time.Second

// localInstancesByHarness returns a {harness: instance_id} map of the
// first enabled local-transport instance for each requested harness type.
// Used by session discovery / auto-resume to land orphaned sessions on
// the local-host instance when one exists.
func (s *Server) localInstancesByHarness(types []msg.Harness) map[msg.Harness]string {
	out := make(map[msg.Harness]string)
	if s.harnessStore == nil {
		return out
	}
	for _, h := range types {
		instances, err := s.harnessStore.ListInstancesByHarness(h)
		if err != nil {
			continue
		}
		for _, inst := range instances {
			if !inst.Enabled {
				continue
			}
			m, err := s.harnessStore.GetMachine(inst.MachineID)
			if err != nil || m.Transport != msg.TransportLocal {
				continue
			}
			out[h] = inst.ID
			break
		}
	}
	return out
}

// ReconcileAndResume clears stale 'running' state left over from the previous
// server lifetime and auto-restarts the harness for sessions whose last
// activity was inside autoResumeWindow. Sessions that went quiet before the
// window are left idle — the user can resume them on demand.
func (s *Server) ReconcileAndResume() {
	sessions, err := s.harness.ReconcileStaleSessions()
	if err != nil {
		log.Printf("[reconcile] %v", err)
		return
	}
	if len(sessions) == 0 {
		return
	}
	log.Printf("[reconcile] reset %d stale running→idle", len(sessions))

	if s.harnessStore == nil {
		return
	}
	cutoff := time.Now().Add(-autoResumeWindow)
	var resumed int
	for i := range sessions {
		sess := sessions[i]
		lastAt, err := s.harness.SessionEvents().LastActivityAt(sess.SessionID)
		if err != nil {
			log.Printf("[reconcile] %s: last-activity lookup failed: %v", sess.SessionID, err)
			continue
		}
		if lastAt.Before(cutoff) {
			continue
		}
		resumed++
		go s.autoResume(sess)
	}
	if resumed > 0 {
		log.Printf("[reconcile] auto-resuming %d sessions active within %s", resumed, autoResumeWindow)
	}
}

// StartWatchdog launches a goroutine that periodically scans for sessions
// the database still marks `running` but for which no harness subprocess is
// registered with the manager. That gap opens when a harness crashes (OOM,
// panic, network drop on SSH-runner) without emitting a terminal `result` or
// `error` event — readEvents removes the process from the manager map, but
// state stays `running` because no terminal event arrived. Without this
// watchdog the session sits hung until the user touches it.
//
// Startup-only ReconcileAndResume covers the cross-restart case; this loop
// covers in-lifetime crashes.
func (s *Server) StartWatchdog() {
	go func() {
		ticker := time.NewTicker(watchdogInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.watchdogTick()
			s.reapIdleTick()
		}
	}()
}

func (s *Server) watchdogTick() {
	if s.harnessStore == nil {
		return
	}
	for _, st := range msg.ActiveSessionStates() {
		sessions, err := s.store.ListSessionsByState(string(st))
		if err != nil {
			log.Printf("[watchdog] list %s sessions: %v", st, err)
			continue
		}
		for i := range sessions {
			sess := sessions[i]
			if s.harness.HasProcess(sess.SessionID) {
				continue
			}
			// A harness subagent never has a process of its own: it runs
			// inside its parent's, so "no process" is its normal state, and
			// autoResume refuses to start one. This used to log "resuming"
			// and force it idle every tick it was mid-task (41 a day), and the
			// resume never happened. While the parent's process runs, the
			// subagent is running; once it is gone, nothing will ever report
			// the task's end, so record that end here.
			// A runner that is away may still be holding this session's
			// process (or, for a subagent, its parent's). It says so when it
			// returns, and until then the session is left as it is.
			if s.harness.AwaitingRunner(sess.SessionID) {
				continue
			}
			if sess.ControlledBy == msg.ControlledByHarness {
				if sess.ManagerSessionID != "" && (s.harness.HasProcess(sess.ManagerSessionID) || s.harness.AwaitingRunner(sess.ManagerSessionID)) {
					continue
				}
				log.Printf("[watchdog] %s: subagent at state=%s, and the process it ran in is gone; recording error (parent_process_exited)", sess.SessionID, st)
				if _, err := s.harness.ForceSessionState(sess.SessionID, msg.SessionError, msg.ReasonParentProcessExited); err != nil {
					log.Printf("[watchdog] %s: state write failed: %v", sess.SessionID, err)
				}
				continue
			}
			log.Printf("[watchdog] %s: state=%s but no harness process; resuming", sess.SessionID, st)
			// Drop back to idle so autoResume's StartOnInstance path takes a
			// clean state transition. Skip the auto-resume on failure to flip
			// state — leaving it active would just refire next tick.
			if _, err := s.harness.ForceSessionState(sess.SessionID, msg.SessionIdle, msg.ReasonWatchdogNoProcess); err != nil {
				log.Printf("[watchdog] %s: state reset failed: %v", sess.SessionID, err)
				continue
			}
			go s.autoResume(sess)
		}
	}
}

// reapIdleTick kills harness processes that have gone quiet past their
// idle timeout and records how each session was left (see
// harness.sessionStateAfterIdleReap: a finished turn reads completed, an
// unanswered question keeps its state, reason idle_reaped either way). This is the source-level
// fix for the warm-process leak: after a turn completes the claude
// subprocess lingers (on stdin in events mode, on the pty fd in pty mode)
// holding ~150MB while it waits for a follow-up turn that one-shot
// autoworkers never send. Left unchecked these accumulate until OOM.
//
// "Quiet" is measured by the most recent event of ANY kind. Both stream
// output and OTel telemetry land in the events table — pty telemetry via
// /sidecar/event → BroadcastEvent → StoreEvent — so a single timestamp
// (LastActivityAt) covers events mode and pty mode alike.
func (s *Server) reapIdleTick() {
	if s.harness == nil {
		return
	}
	now := time.Now()
	for _, id := range s.harness.ListActiveSessions() {
		sess, err := s.store.GetSession(id)
		if err != nil || sess == nil {
			continue
		}

		// Last activity = most recent event of any kind. Zero means no
		// events yet (just spawned); the decision falls back to the row's
		// updated_at so a fresh process isn't reaped before its first event.
		lastAt, err := s.harness.SessionEvents().LastActivityAt(id)
		if err != nil {
			log.Printf("[reaper] %s: last-activity lookup failed: %v", id, err)
			continue
		}

		// A session that left ListActiveSessions since the list was taken
		// has no process to reap.
		processStartedAt, running := s.harness.ProcessStartedAt(id)
		if !running {
			continue
		}
		// Only this process's wakeup counts: one set by an earlier process
		// died with it, and redeliverWakeupsLostInRestart owns that case.
		wakeup, err := s.harness.SessionEvents().LatestScheduledWakeup(id, processStartedAt)
		if err != nil {
			log.Printf("[reaper] %s: pending-wakeup lookup failed: %v", id, err)
			continue
		}
		var wakeupDueAt time.Time
		if wakeup != nil {
			wakeupDueAt = wakeup.DueAt
		}

		quiet, reap := reapDecision(now, sess.Mode, msg.SessionState(sess.State), lastAt, sess.UpdatedAt, wakeupDueAt, s.settings.Duration(config.SettingSessionIdleTimeout), s.settings.Duration(config.SettingSessionPTYIdleTimeout))
		if !reap {
			continue
		}

		// Say which of the two faults this is. A session reaped out of
		// `starting` never emitted a first event, so calling it "idle" hides
		// the spawn failure behind a word that means the opposite.
		if msg.SessionState(sess.State) == msg.SessionStarting {
			log.Printf("[reaper] %s: spawned %s ago and never emitted a first event (mode=%s); killing", id, quiet.Round(time.Second), sess.Mode)
		} else {
			log.Printf("[reaper] %s: idle %s (mode=%s state=%s) exceeds timeout; killing", id, quiet.Round(time.Second), sess.Mode, sess.State)
		}
		// The kill is unconditional; ReapIdleProcess decides only what the
		// row says afterwards (completed for a finished turn, the question
		// kept for an unanswered one), always with reason idle_reaped.
		left, err := s.harness.ReapIdleProcess(id)
		if err != nil {
			log.Printf("[reaper] %s: killed, but recording state %s failed: %v", id, left, err)
			continue
		}
		log.Printf("[reaper] %s: killed; recorded %s (idle_reaped)", id, left)
	}
}

// reapDecision is the pure idle-reaper policy, split out so the gating
// rules are unit-testable without a live harness. Returns how long the
// session has been quiet and whether it should be killed.
//
// Rules:
//   - Pick the timeout by mode (pty uses the longer PTYIdleTimeout).
//   - timeout <= 0 disables reaping for that mode.
//   - Events-mode sessions the harness has reported busy (model_generating,
//     tool_running, compacting, rate_limited, running) are never reaped:
//     a long tool call emits no events while it runs, so state — not the
//     timestamp — is the guard. PTY state isn't derived from telemetry, so
//     PTY is intentionally not state-gated; its longer timeout is the guard.
//   - `starting` is NOT one of those, and that is the point. Every other
//     active state was set BY the harness, so it is evidence the process is
//     alive and silence is expected. `starting` is set by the bridge at spawn
//     and means the exact opposite: nothing has been heard from the harness
//     at all. Exempting it turned a spawn that never emits a first event into
//     a session parked in `starting` forever, with Send disabled behind it and
//     no error anywhere. So `starting` is reaped on the ordinary timeout.
//   - In `starting` the clock runs from updatedAt — the moment the state was
//     entered, i.e. the spawn — and NEVER from the last event. A resumed
//     session's newest event belongs to its previous process and is already
//     hours old, so measuring from it would reap the new process on the first
//     tick instead of giving it the grace gap it is owed.
//   - Otherwise idle is measured from the last event, falling back to
//     updatedAt when no event has landed yet.
//   - A pending wakeup counts as activity at the time it comes due, so the
//     clock runs from whichever is later. An agent that ends its turn with a
//     ScheduleWakeup is waiting, not done, and the wakeup's timer lives in its
//     process: reaping it before the wakeup comes due loses the wakeup with
//     no error anywhere (br_1790185922026535143, 2026-09-28, reaped two
//     minutes before its 20-minute check). A wakeup that comes due and does
//     not fire leaves the session reaped one timeout later, as usual.
func reapDecision(now time.Time, mode msg.SessionMode, state msg.SessionState, lastActivity, updatedAt, pendingWakeupDueAt time.Time, idleTimeout, ptyTimeout time.Duration) (time.Duration, bool) {
	isPTY := mode == msg.SessionModePTY
	timeout := idleTimeout
	if isPTY {
		timeout = ptyTimeout
	}
	if timeout <= 0 {
		return 0, false
	}
	starting := state == msg.SessionStarting
	if !isPTY && state.IsActive() && !starting {
		return 0, false
	}
	ref := lastActivity
	if starting || ref.IsZero() {
		ref = updatedAt
	}
	if !starting && pendingWakeupDueAt.After(ref) {
		ref = pendingWakeupDueAt
	}
	quiet := now.Sub(ref)
	return quiet, quiet >= timeout
}

// autoResume restarts a single session's harness process. Mirrors the flow in
// handleResumeSession minus the HTTP plumbing. If the previous turn was
// killed mid-flight (a user_message with no following result), the message
// text is re-sent once the harness is ready so the turn actually completes
// instead of sitting idle waiting for new input.
func (s *Server) autoResume(sess store.Session) {
	// A session the bridge does not control must never be started by the
	// bridge. A harness subagent has no process of its own to reconcile — it
	// lives inside its parent's — so "running but no process" is its NORMAL
	// state between the parent's frames, not a fault to repair.
	//
	// Resuming one is not a no-op, it is destructive: observed in production,
	// the watchdog resumed a promoted subagent, the adapter refused to
	// `--resume agent-<task_id>` because that is not a Claude Code UUID and
	// started a FRESH agent instead, that agent replayed the turn and began
	// running tools unsupervised, and the new session's UUID overwrote the
	// row's harness_session_id — destroying the dedupe key discovery uses to
	// converge on the same row.
	//
	// TEAM-ORCHESTRATION §21.6 called this out: controlled_by was written by
	// the promotion path and read by nothing, and became load-bearing the
	// moment something tried to resume a subagent. This is that gate.
	if sess.ControlledBy == msg.ControlledByHarness {
		return
	}
	if sess.InstanceID == "" {
		// Every code path that creates a session populates instance_id (see
		// handleCreateSession; resolveInstance fails the request otherwise).
		// Discovered sessions can land with instance_id="" if no local instance
		// is enabled for their harness — those should never reach `running`,
		// so getting here means a real invariant break we want to see.
		log.Printf("[auto-resume] %s: ERROR instance_id empty — session cannot be resumed; skipping", sess.SessionID)
		return
	}
	if over, spendUSD, maxBudgetUSD := s.harness.SessionOverBudget(sess.SessionID); over {
		log.Printf("[auto-resume] %s: not resuming — spent $%.2f of its $%.2f ceiling", sess.SessionID, spendUSD, maxBudgetUSD)
		return
	}
	inst, err := s.harnessStore.GetInstance(sess.InstanceID)
	if err != nil {
		log.Printf("[auto-resume] %s: instance %s not found: %v", sess.SessionID, sess.InstanceID, err)
		return
	}
	credID := s.harnessStore.FirstEnabledCredentialID(inst.ID)
	if _, err := s.StartOnInstance(context.Background(), &sess, inst, credID); err != nil {
		log.Printf("[auto-resume] %s: start failed: %v", sess.SessionID, err)
		return
	}
	log.Printf("[auto-resume] %s: resumed", sess.SessionID)

	turn, err := s.harness.InterruptedTurn(sess.SessionID)
	if err != nil {
		log.Printf("[auto-resume] %s: interrupted-turn check failed: %v", sess.SessionID, err)
		return
	}
	if turn == nil {
		s.tellResumedSessionItsBackgroundTasksDied(sess)
		return
	}
	// Harness subprocess needs a moment to finish its start handshake before
	// it will accept a message on stdin. 2s is enough for Claude Code's
	// resume-load; shorter risks writing before the pipe is being drained.
	time.Sleep(2 * time.Second)
	text, replayed := resumeMessage(*turn)
	if err := s.harness.Send(sess.SessionID, text, nil); err != nil {
		log.Printf("[auto-resume] %s: resume send failed: %v", sess.SessionID, err)
		return
	}
	if replayed {
		log.Printf("[auto-resume] %s: replayed interrupted turn", sess.SessionID)
		return
	}
	log.Printf("[auto-resume] %s: turn was interrupted after %d tool calls; sent an interruption notice instead of replaying the instruction",
		sess.SessionID, turn.ToolCallsAlreadyRun)
}

// tellResumedSessionItsBackgroundTasksDied handles the resume that has no
// interrupted turn to pick up: the last turn finished, and what the restart
// killed was the work that turn left running — subagents, backgrounded
// commands. sess is the row as it read before the reconcile reset it, so its
// state still says what the session was doing when the process went away.
//
// Those tasks do not come back with the process. The model is parked waiting
// for them to report, and nothing ever will, so unless it is told it waits for
// good — resumed, idle, and looking finished.
func (s *Server) tellResumedSessionItsBackgroundTasksDied(sess store.Session) {
	if msg.SessionState(sess.State) != msg.SessionBackgroundTasksRunning {
		return
	}
	tasks, err := s.harness.SessionEvents().LastReportedBackgroundTasks(sess.SessionID)
	if err != nil {
		log.Printf("[auto-resume] %s: reading the background tasks it lost failed: %v", sess.SessionID, err)
		return
	}
	// Same wait as the interrupted-turn send below: the start handshake.
	time.Sleep(2 * time.Second)
	if err := s.harness.Send(sess.SessionID, backgroundTasksLostMessage(tasks), nil); err != nil {
		log.Printf("[auto-resume] %s: background-tasks notice send failed: %v", sess.SessionID, err)
		return
	}
	log.Printf("[auto-resume] %s: its turn had finished but %d background task(s) died with the process; sent a notice naming them",
		sess.SessionID, len(tasks))
}

// backgroundTasksLostMessage is what a resumed session is told when the restart
// killed work its finished turn had left running. It names each task, because
// "some tasks" leaves the model to guess which results it is still owed. It is a
// <bridge-report kind="restart"> block, so the chat folds it rather than drawing
// it as the person's message; so is resumeMessage's notice.
func backgroundTasksLostMessage(tasks []msg.BackgroundTask) string {
	var b strings.Builder
	b.WriteString("[llm-bridge] The harness process was restarted under this session and it has just been " +
		"resumed. Your last turn had finished, but background work it started was still running " +
		"inside that process and was killed with it. It will not report back on its own, so do not wait for it.\n\n")
	if len(tasks) == 0 {
		b.WriteString("The bridge has no list of what was running.\n\n")
	} else {
		b.WriteString("What was running:\n")
		for _, task := range tasks {
			fmt.Fprintf(&b, "- %s (%s, task %s)\n", task.Description, task.TaskType, task.TaskID)
		}
		b.WriteString("\n")
	}
	b.WriteString("This notice is not a new instruction; the user's last request still stands. Your transcript " +
		"above is intact, and so is each subagent's: resume a subagent you still need by sending it a " +
		"message (in Claude Code, SendMessage to its task id), and it carries on from where it stopped. " +
		"The restart stopped it, not the user. If the harness refuses the resume or calls it stopped by " +
		"the user, start a new agent for the same task: the user's request already covers that, so do " +
		"not stop to ask. A backgrounded command may have got part way, so check the real state of " +
		"anything it could have changed before running it again. Do not repeat work that already completed.")
	return msg.WrapBridgeReport(msg.BridgeReportKindRestart, b.String())
}

// resumeMessage decides what to say to a harness that has just come back up
// under an interrupted turn, and reports whether that text is the original
// instruction replayed verbatim.
//
// Something must be said either way: the harness resumes holding the whole
// conversation but idle, waiting on stdin, so a turn killed mid-flight never
// finishes on its own.
//
// What to say depends on whether the turn had already done anything. A turn
// killed before its first tool call has left nothing behind, so replaying the
// instruction verbatim is exactly right — it asks for work that has not
// happened yet.
//
// A turn killed after its tool calls is a different thing wearing the same
// shape. Its work is partly on disk already, and the instruction replayed
// verbatim is indistinguishable from the user asking for all of it a second
// time. Observed on this host: a deploy turn was killed by the restart its own
// deploy triggered, and auto-resume handed the approval back as if freshly
// typed; nothing but the model re-reading its own transcript stood between
// that and a second deploy. So for that case say what happened and let the
// model reconcile against the transcript it still holds, rather than reissuing
// an order whose side effects have already partly landed.
func resumeMessage(turn store.InterruptedTurn) (text string, replayedVerbatim bool) {
	if turn.ToolCallsAlreadyRun == 0 {
		return turn.UserMessageText, true
	}
	return msg.WrapBridgeReport(msg.BridgeReportKindRestart, fmt.Sprintf(
		"[llm-bridge] Your previous turn did not finish: the harness process was restarted "+
			"under it and this session has just been resumed. %d tool call(s) from that turn "+
			"had already run, so part of its work may have landed and part of it certainly did "+
			"not.\n\n"+
			"This notice is not a new instruction, and the user's last message is deliberately "+
			"not repeated here — treat it as still standing, not as newly given. Your own "+
			"transcript above is intact: read back what you had already done, check the real "+
			"state of anything it changed rather than assuming either way, and carry on from "+
			"there. Do not repeat a step that already completed.",
		turn.ToolCallsAlreadyRun)), false
}

// AutoDiscover runs session discovery for all harness types and imports them to the store.
// AutoDiscover imports once; WatchExternalHarnessActivity handles later changes.
func (s *Server) AutoDiscover() {
	go func() {
		if err := s.discoverOnce(context.Background()); err != nil {
			log.Printf("auto-discover: %v", err)
		}
	}()
}

func (s *Server) discoverOnce(ctx context.Context) error {
	s.discoveryMu.Lock()
	defer s.discoveryMu.Unlock()
	if err := s.validateDiscoveryPrincipal(ctx); err != nil {
		return err
	}
	sessions, err := s.harness.DiscoverSessions(ctx, "")
	if err != nil {
		return err
	}

	// Build map of harness type → local instance ID.
	// Discovery runs the harness binary locally, so sessions belong to the local instance.
	localInstances := s.localInstancesByHarness([]msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex})

	// Announce the blast radius BEFORE writing any of it.
	//
	// Whether a discovered session is "new" is decided by this instance's
	// own DB, but the import it triggers writes into log-store, which this
	// instance does not own and cannot clean up. So the guard against
	// re-importing a transcript lives in a database log-store cannot see:
	// point the binary at a fresh DB and every session on disk is new
	// again, and every one of their transcripts is written into the shared
	// event log a second time under a fresh bridge id.
	//
	// That happened on 2026-08-01: a canary booted against an empty
	// /tmp DB re-imported 2,863 sessions into the production log-store in
	// two minutes, and the throwaway DB that would have identified them
	// was deleted minutes later. The per-session lines below arrive after
	// the fact and 2,510 times over, and none of them names log-store —
	// so this one line, before the loop, is the only warning an operator
	// polling for readiness can actually catch.
	if len(sessions) > 0 {
		log.Printf("[auto-discover] %d sessions on disk; any not already in this instance's DB will have their transcripts imported into the session log",
			len(sessions))
	}

	var imported, linkedCount, reclassified, filled int
	var pendingLinks []discoveredLink
	for _, ds := range sessions {
		displayName := displayNameForDiscoveredSession(ds.Prompt, ds.Project)

		instanceID := localInstances[ds.Harness]
		// Prefer the adapter's structural source tag (e.g. claudecode marks
		// Task()-spawned subagents from the on-disk layout) over our prompt-
		// prefix heuristic. Fall back to prefix inference only when the
		// adapter has no structural signal.
		source, folder := ds.Source, s.folderForPurpose(ds.Source)
		if source == "" {
			source, folder = s.discoverySourceFolder(ds.Prompt)
		}
		bridgeID, inserted, err := s.store.UpsertDiscoveredSessionOwnedBy(ds.HarnessSessionID, ds.BridgeSessionID, displayName, string(ds.Harness), instanceID, source, folder, ds.CreatedAt, ds.UpdatedAt, s.cfg.DiscoveryPrincipalID)
		// Lineage is resolved after every row exists, not here: discovery
		// has no parent-before-child ordering, so linking inline drops
		// every subagent that happened to be walked before its parent —
		// 497 of 1,292 on the first real run.
		if err == nil && ds.ParentHarnessSessionID != "" {
			pendingLinks = append(pendingLinks, discoveredLink{bridgeID: bridgeID, parentHarnessID: ds.ParentHarnessSessionID})
		}
		if err != nil {
			log.Printf("[auto-discover] failed to upsert session %s: %v", ds.HarnessSessionID, err)
			continue
		}
		if s.fillOrRepairDiscoveredSessionMetadata("[auto-discover]", bridgeID, ds) {
			filled++
		}
		if !inserted {
			changed, err := s.reclassifyDiscoveredSession(bridgeID, ds.Source)
			if err != nil {
				log.Printf("[auto-discover] failed to reclassify %s as %q: %v", bridgeID, ds.Source, err)
			} else if changed {
				reclassified++
			}
		}
		if inserted {
			imported++
			// Import history to log-store for new sessions
			func(h msg.Harness, brID, sid string) {
				n, err := s.harness.ImportHistory(ctx, brID, h, sid)
				if err != nil {
					log.Printf("[auto-discover] failed to import history for %s: %v", sid, err)
				} else if n > 0 {
					log.Printf("[auto-discover] imported %d events for session %s", n, sid)
				}
			}(ds.Harness, bridgeID, ds.HarnessSessionID)
		}
	}
	if imported > 0 {
		log.Printf("[auto-discover] imported %d sessions", imported)
	}
	if reclassified > 0 {
		log.Printf("[auto-discover] reclassified %d existing discovered sessions to the purpose their adapter now reports", reclassified)
	}
	log.Printf("[auto-discover] filled a working folder or replaced a blank or harness-injected title on %d sessions", filled)
	for _, l := range pendingLinks {
		linked, err := s.store.LinkDiscoveredSessionParent(l.bridgeID, l.parentHarnessID)
		if err != nil {
			log.Printf("[auto-discover] failed to link %s to parent %s: %v", l.bridgeID, l.parentHarnessID, err)
		} else if linked {
			linkedCount++
		}
	}
	if linkedCount > 0 {
		log.Printf("[auto-discover] linked %d sessions to the parent that spawned them", linkedCount)
	}
	return nil
}

// discoveredLink is a lineage link waiting for its parent row to exist.
//
// Discovery walks harness storage in whatever order the filesystem yields, so a
// subagent is routinely seen before the session that spawned it. Linking inline
// therefore silently drops every such child — 497 of 1,292 on the first real
// run. Collecting the links and resolving them once the pass has upserted every
// row removes the ordering dependency entirely.
type discoveredLink struct {
	bridgeID        string
	parentHarnessID string
}

// delegateRequestRefusal holds a create request from a session's agent to its
// own session: the delegate sits under that session and runs as its
// principal. It fills in either when the request leaves it empty, and refuses
// a request that names another, so a caller that asked for more is told so.
func delegateRequestRefusal(req *CreateSessionRequest, agentSessionID, agentPrincipalID string) *requestcaller.CredentialError {
	if req.ManagerSessionID != "" && req.ManagerSessionID != agentSessionID {
		return requestcaller.NewCredentialError(http.StatusForbidden, "delegate_manager_mismatch", fmt.Sprintf(
			"manager_session_id %q is not %s, the session this posting token belongs to; an agent starts delegates only under its own session (omit manager_session_id)",
			req.ManagerSessionID, agentSessionID))
	}
	if req.PrincipalID != "" && req.PrincipalID != agentPrincipalID {
		own := "no principal"
		if agentPrincipalID != "" {
			own = "principal " + agentPrincipalID
		}
		return requestcaller.NewCredentialError(http.StatusForbidden, "delegate_principal_mismatch", fmt.Sprintf(
			"principal_id %q is not the principal of session %s, which runs with %s; a delegate runs with its session's principal and no other (omit principal_id)",
			req.PrincipalID, agentSessionID, own))
	}
	req.ManagerSessionID = agentSessionID
	req.PrincipalID = agentPrincipalID
	return nil
}

// bundleStoreOffMessage says why a server has no bundle-store.
const bundleStoreOffMessage = "this server's bundle-store module is off (" + config.PostgresURLEnvironmentVariable + " is not set)"
