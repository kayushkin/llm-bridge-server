package store

// A benchmark of the reads of the bridge's own records that the server makes
// on its busiest routes: the session list (the sidebar's pages, filters and
// lookups), one session, briefs, open signals, a session's buttons and the
// stored settings.
//
//	export POSTGRESTEST_ADMINISTRATOR_URL=<a Postgres with default durability>
//	go test ./internal/store -run '^$' -bench BenchmarkBridgeCoreReads -benchtime 1x -timeout 60m
//
// The store is built by openBridgeCoreReadBenchmarkStore. The same file runs
// against the SQLite store of the build before the move, with that
// constructor, so the two compare like with like. The counts are of the live
// bridge database on 2026-11-25, scaled down: 86,191 sessions, 3,824 signals,
// 351 buttons and 197 briefs. Every record here is made up.

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const (
	coreBenchmarkSessions   = 30000
	coreBenchmarkParents    = 300
	coreBenchmarkPrincipals = 3
	coreBenchmarkBriefs     = 200
	coreBenchmarkSignals    = 1500
	coreBenchmarkActions    = 20
)

func coreBenchmarkSessionID(index int) string { return fmt.Sprintf("br_core_benchmark_%06d", index) }

func populateCoreBenchmark(b *testing.B, s *Store) {
	types := []msg.SessionType{msg.SessionTypeInteractive, msg.SessionTypeAutonomous, msg.SessionTypeSystem, msg.SessionTypeExternal}
	for index := 0; index < coreBenchmarkSessions; index++ {
		session := &Session{
			SessionID:   coreBenchmarkSessionID(index),
			DisplayName: fmt.Sprintf("made-up session %d", index),
			Harness:     msg.HarnessClaudeCode,
			InstanceID:  "inst-made-up",
			State:       string(msg.SessionIdle),
			Type:        types[index%len(types)],
			Purpose:     "chat",
			Origin:      "benchmark",
			PrincipalID: fmt.Sprintf("principal_%06d", index%coreBenchmarkPrincipals+1),
			WorkingDir:  "/work/made-up",
		}
		if index >= coreBenchmarkParents && index%10 == 0 {
			session.ManagerSessionID = coreBenchmarkSessionID(index % coreBenchmarkParents)
			session.Purpose = "subagent"
		}
		if err := s.CreateSession(session); err != nil {
			b.Fatal(err)
		}
	}
	for index := 0; index < coreBenchmarkBriefs; index++ {
		sessionID := coreBenchmarkSessionID(index)
		for turn := 0; turn < 7; turn++ {
			update := SessionBriefUpdate{
				SessionID: sessionID,
				Revision: msg.SessionBriefRevision{ID: fmt.Sprintf("brief_revision_%d_%d", index, turn), OverallGoal: "a made-up goal",
					CurrentStep: "a made-up step", TurnChange: "made up"},
			}
			if turn%2 == 0 {
				update.Raised = []msg.LooseEnd{{ID: fmt.Sprintf("loose_end_%d_%d", index, turn), Kind: msg.LooseEndKinds[0], Text: "a made-up question"}}
			}
			if err := s.ApplySessionBriefUpdate(update); err != nil {
				b.Fatal(err)
			}
		}
	}
	for index := 0; index < coreBenchmarkSignals; index++ {
		state := msg.SignalStateOpen
		if index%3 != 0 {
			state = msg.SignalStateAnswered
		}
		if err := s.CreateSignal(&msg.Signal{ID: fmt.Sprintf("sig_core_benchmark_%d", index), SessionID: coreBenchmarkSessionID(index * 7 % coreBenchmarkSessions),
			Kind: msg.SignalKindQuestion, Source: msg.SignalSourceTool, Surface: msg.SignalSurfaceKanban, Title: "a made-up question", State: state}); err != nil {
			b.Fatal(err)
		}
	}
	for index := 0; index < coreBenchmarkActions; index++ {
		action := SessionAction{
			SessionID: coreBenchmarkSessionID(0),
			Offer:     msg.SessionActionOffer{Label: "List messages", Type: msg.SessionActionRunCommand, ShellCommand: "true"},
			Command:   "true",
			State:     msg.SessionActionOffered,
			OfferedAt: time.Now().UTC(),
		}
		if err := s.InsertSessionAction(&action); err != nil {
			b.Fatal(err)
		}
	}
}

// coreReadMilliseconds is the quantiles of repetitions of one read.
func timeCoreRead(b *testing.B, measurement string, repetitions int, read func() error) {
	if err := read(); err != nil {
		b.Fatalf("%s: %v", measurement, err)
	}
	durations := make([]time.Duration, 0, repetitions)
	for range repetitions {
		started := time.Now()
		if err := read(); err != nil {
			b.Fatalf("%s: %v", measurement, err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	quantile := func(q float64) float64 {
		return float64(durations[int(q*float64(len(durations)-1))].Microseconds()) / 1000
	}
	fmt.Printf("| core reads | %s | n=%d p50=%.2fms p99=%.2fms max=%.2fms |\n", measurement, repetitions, quantile(0.5), quantile(0.99), quantile(1))
}

func BenchmarkBridgeCoreReads(b *testing.B) {
	s := openBridgeCoreReadBenchmarkStore(b)
	populated := time.Now()
	populateCoreBenchmark(b, s)
	settleBridgeCoreReadBenchmarkStore(b, s)
	fmt.Printf("| core reads | populate %d sessions, %d briefs, %d signals | %.1fs |\n", coreBenchmarkSessions, coreBenchmarkBriefs, coreBenchmarkSignals, time.Since(populated).Seconds())

	timeCoreRead(b, "session list, first page of 100", 50, func() error {
		_, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{})
		return err
	})
	timeCoreRead(b, "session list, tenth page of 100", 20, func() error {
		cursor := ""
		for range 10 {
			rows, err := s.ListSessionSummaries(100, cursor, SessionSummaryFilter{})
			if err != nil {
				return err
			}
			cursor = rows[len(rows)-1].Cursor
		}
		return nil
	})
	timeCoreRead(b, "session list, one principal's interactive sessions", 50, func() error {
		_, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{OwnedByPrincipalID: "principal_000002", Types: []string{string(msg.SessionTypeInteractive)}})
		return err
	})
	parents := make([]string, 50)
	names := make([]string, 50)
	for index := range parents {
		parents[index] = coreBenchmarkSessionID(index)
		names[index] = coreBenchmarkSessionID(index * 503 % coreBenchmarkSessions)
	}
	timeCoreRead(b, "session list, children of 50 parents", 50, func() error {
		_, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{ManagerSessionIDs: parents})
		return err
	})
	timeCoreRead(b, "session list, 50 sessions by id", 50, func() error {
		_, err := s.ListSessionSummaries(100, "", SessionSummaryFilter{SessionIDs: names})
		return err
	})
	timeCoreRead(b, "session list revision and counts by principal", 50, func() error {
		if _, err := s.MaxSessionUpdatedAt(); err != nil {
			return err
		}
		_, err := s.SessionCountsByPrincipal("", "")
		return err
	})
	timeCoreRead(b, "full sessions, newest 100", 50, func() error {
		_, err := s.ListSessionsPaged(100, 0)
		return err
	})
	timeCoreRead(b, "one session", 200, func() error {
		_, err := s.GetSession(coreBenchmarkSessionID(12345))
		return err
	})
	timeCoreRead(b, "session briefs, newest 50 with open loose ends", 50, func() error {
		_, err := s.ListSessionBriefs(SessionBriefFilter{Limit: 50, LooseEndState: msg.LooseEndOpen})
		return err
	})
	timeCoreRead(b, "one session's brief", 200, func() error {
		_, err := s.GetSessionBriefView(coreBenchmarkSessionID(7))
		return err
	})
	timeCoreRead(b, "open signals", 50, func() error {
		_, err := s.ListSignals(SignalFilter{State: msg.SignalStateOpen})
		return err
	})
	timeCoreRead(b, "a session's buttons", 200, func() error {
		_, err := s.ListSessionActions(coreBenchmarkSessionID(0))
		return err
	})
	timeCoreRead(b, "stored settings", 200, func() error {
		_ = s.ServiceSettingValues()
		return nil
	})
	timeCoreRead(b, "peer connections", 200, func() error {
		_, err := s.ListPeerConnections()
		return err
	})
}
