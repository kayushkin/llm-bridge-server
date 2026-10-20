package server

import (
	"context"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// The external activity watcher brings into the dash what a person does with
// claude or codex in a terminal while this server is up: a turn in a
// conversation the bridge already has (`claude --resume <id>`, `codex resume
// <id>`), and a conversation started there. AutoDiscover covers the files once,
// at startup; until 2026-10-20 nothing covered them after that, so a terminal
// turn reached the dash only at the next restart, and then only for a session
// the bridge had never seen.
//
// Each pass asks every local harness wrapper which session files changed since
// the previous pass (`-changed-since`; a stat walk that parses only the changed
// files, where -discover parses all of them). For each:
//
//   - a session the bridge has no row for is recorded as discovery records one,
//     and its history imported into log-store;
//   - a session the bridge has a process for, or a process still exiting, is
//     skipped: what the harness writes is the bridge's own and already
//     reported. So is a harness subagent whose parent has one;
//   - otherwise its transcript is read and every event written after the
//     session's mark goes through BroadcastEvent, as a pty sidecar's events
//     do, so the chat, log-store and the session's state all see the turn. The
//     mark then moves to the newest event read.
//
// A session's mark is where the dash already has it: set when the bridge's own
// process for it finishes exiting, and moved by each import. A session with no
// mark starts from its last stored event (rounded up to the second the events
// table keeps), or from when the watcher started if it has none.

// externalActivityPassOverlap re-asks for a little before the previous pass,
// so a file written while the previous pass ran is not missed. Re-reading it is
// harmless: the mark keeps an event from being imported twice.
const externalActivityPassOverlap = 2 * time.Second

// WatchExternalHarnessActivity runs the watcher until ctx ends. The interval
// is read from external_activity.poll_interval before each pass, so a change
// takes effect without a restart; 0s or less pauses it.
func (s *Server) WatchExternalHarnessActivity(ctx context.Context) {
	started := time.Now()
	since := started
	lastErrors := map[msg.Harness]string{}
	for {
		interval := s.settings.Duration(config.SettingExternalActivityPollInterval)
		wait := interval
		if wait <= 0 {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if interval <= 0 {
			continue
		}
		passStart := time.Now()
		for _, h := range []msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex} {
			err := s.importExternalActivity(ctx, h, since.Add(-externalActivityPassOverlap), started)
			if ctx.Err() != nil {
				return // stopped mid-pass; the killed wrapper is not an error
			}
			// Said once per change of error, not every pass.
			message := ""
			if err != nil {
				message = err.Error()
			}
			if message != lastErrors[h] {
				if err != nil {
					log.Printf("[external-activity] ERROR %s: %v", h, err)
				} else {
					log.Printf("[external-activity] %s: working again", h)
				}
				lastErrors[h] = message
			}
		}
		since = passStart
	}
}

// importExternalActivity is one pass for one harness.
func (s *Server) importExternalActivity(ctx context.Context, h msg.Harness, since, watcherStarted time.Time) error {
	changed, err := s.harness.ChangedSessions(ctx, h, since)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}
	localInstances := s.localInstancesByHarness([]msg.Harness{h})
	for _, ds := range changed {
		if ds.HarnessSessionID == "" {
			continue
		}
		row, err := s.store.GetSessionByHarnessSessionID(ds.HarnessSessionID)
		if err != nil || row == nil {
			s.recordExternalSession(ctx, ds, localInstances[h])
			continue
		}
		if s.harness.HasProcessOrCleanup(row.SessionID) {
			continue
		}
		// A harness subagent runs inside its parent's process, which reports
		// it; only a subagent of a conversation run outside the bridge is
		// this watcher's to import.
		if row.ControlledBy == msg.ControlledByHarness && row.ParentID != "" && s.harness.HasProcessOrCleanup(row.ParentID) {
			continue
		}
		if err := s.importExternalTurns(ctx, h, row.SessionID, ds.HarnessSessionID, watcherStarted); err != nil {
			log.Printf("[external-activity] ERROR session %s (%s %s): %v", row.SessionID, h, ds.HarnessSessionID, err)
		}
	}
	return nil
}

// recordExternalSession records a conversation started outside the bridge,
// exactly as AutoDiscover records one it finds at startup.
func (s *Server) recordExternalSession(ctx context.Context, ds msg.StoredSession, instanceID string) {
	source, folder := ds.Source, s.folderForPurpose(ds.Source)
	if source == "" {
		source, folder = s.discoverySourceFolder(ds.Prompt)
	}
	bridgeID, inserted, err := s.store.UpsertDiscoveredSession(ds.HarnessSessionID, ds.BridgeSessionID, displayNameForDiscoveredSession(ds.Prompt, ds.Project), string(ds.Harness), instanceID, source, folder, ds.CreatedAt, ds.UpdatedAt)
	if err != nil {
		log.Printf("[external-activity] ERROR record %s session %s: %v", ds.Harness, ds.HarnessSessionID, err)
		return
	}
	if !inserted {
		return
	}
	if ds.ParentHarnessSessionID != "" {
		if _, err := s.store.LinkDiscoveredSessionParent(bridgeID, ds.ParentHarnessSessionID); err != nil {
			log.Printf("[external-activity] ERROR link %s to parent %s: %v", bridgeID, ds.ParentHarnessSessionID, err)
		}
	}
	n, err := s.harness.ImportHistory(ctx, bridgeID, ds.Harness, ds.HarnessSessionID)
	if err != nil {
		log.Printf("[external-activity] ERROR import history of new session %s (%s): %v", bridgeID, ds.HarnessSessionID, err)
		return
	}
	// The whole transcript is in log-store now; later turns import from here.
	if err := s.store.AdvanceExternalActivityMark(bridgeID, ds.UpdatedAt); err != nil {
		log.Printf("[external-activity] ERROR mark new session %s: %v", bridgeID, err)
	}
	log.Printf("[external-activity] recorded %s session %s started outside the bridge as %s (%d events)", ds.Harness, ds.HarnessSessionID, bridgeID, n)
}

// importExternalTurns broadcasts the events of a known session written after
// its mark.
func (s *Server) importExternalTurns(ctx context.Context, h msg.Harness, bridgeID, harnessSessionID string, watcherStarted time.Time) error {
	mark, err := s.externalActivityStartingMark(bridgeID, watcherStarted)
	if err != nil {
		return err
	}
	events, err := s.harness.HistoryEvents(ctx, h, harnessSessionID)
	if err != nil {
		return err
	}
	newest := mark
	imported := 0
	for i := range events {
		ev := events[i]
		if !ev.Timestamp.After(mark) {
			continue
		}
		ev.BridgeSessionID = bridgeID
		if _, err := s.harness.BroadcastEvent(&ev); err != nil {
			return err
		}
		imported++
		if ev.Timestamp.After(newest) {
			newest = ev.Timestamp
		}
	}
	if imported == 0 {
		return nil
	}
	if err := s.store.AdvanceExternalActivityMark(bridgeID, newest); err != nil {
		return err
	}
	log.Printf("[external-activity] session %s: %d events from a %s run outside the bridge", bridgeID, imported, h)
	return nil
}

// externalActivityStartingMark is the session's mark, or for a session with
// none, where the bridge last stored an event for it.
func (s *Server) externalActivityStartingMark(bridgeID string, watcherStarted time.Time) (time.Time, error) {
	mark, ok, err := s.store.ExternalActivityMark(bridgeID)
	if err != nil || ok {
		return mark, err
	}
	last, err := s.store.LastActivityAt(bridgeID)
	if err != nil {
		return time.Time{}, err
	}
	if last.IsZero() {
		return watcherStarted, nil
	}
	// The events table keeps whole seconds, so the last event was at most
	// one second after what it says.
	return last.Add(time.Second), nil
}
