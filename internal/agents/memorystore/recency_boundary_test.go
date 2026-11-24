package memorystore

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// This package turns a timestamp into a number in three places, and each one
// is a step function rather than a curve:
//
//	builder.go     calculateScore    daysSinceAccess < 1 -> +0.2, < 7 -> +0.1
//	prepare.go     loadRecentFiles   ageMinutes < 60 -> 0.7, < 360 -> 0.6, else 0.5
//	management.go  DecayImportance   last_accessed < now-24h -> importance * 0.99
//
// The first and the third are the same threshold — one day since last access —
// written twice as independent literals, in different units, with no shared
// constant between them. That is why this file covers a mechanism rather than
// the two functions the card named: moving one authoring of "a day" and not the
// other is a change nothing here would have reported.
//
// All three were exercised on every run of the suite before these tests and
// not one cut was pinned, because every fixture sat on the same side of it:
// the builder corpora all pass `lastAccessed: now`, the recent-files fixture
// asserted `m.Importance < 0.4 || m.Importance > 0.8` — a range wide enough to
// hold all three of 0.5, 0.6 and 0.7 — and the decay fixture had a single row
// two days old checked only for having gone down. So every literal in these
// three blocks was free to move and every cut was free to slide, under a green
// suite that named each of them in its prose. Card `3c18632a`.
//
// The rows below straddle each cut: one input on each side, close enough that
// moving the literal by one unit changes an answer.

// scoreTolerance is float noise, not a margin. Every want below is a sum of
// float64 literals the code itself adds, so the only difference a passing
// comparison can absorb is the last bit or two.
const scoreTolerance = 1e-9

// TestRecencyBonusStraddlesTheOneDayAndOneWeekCuts pins both recency cuts and
// both bonuses in calculateScore.
//
// Importance is 0 and the tag set is empty, so the returned score IS the
// recency bonus and nothing else can absorb a change to it.
func TestRecencyBonusStraddlesTheOneDayAndOneWeekCuts(t *testing.T) {
	now := time.Now()
	oneWeek := 7 * 24 * time.Hour

	cases := []struct {
		name         string
		lastAccessed time.Time
		want         float64
	}{
		{"a minute inside one day", now.Add(-24*time.Hour + time.Minute), 0.2},
		{"a minute outside one day", now.Add(-24*time.Hour - time.Minute), 0.1},
		{"an hour inside one week", now.Add(-oneWeek + time.Hour), 0.1},
		{"an hour outside one week", now.Add(-oneWeek - time.Hour), 0.0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := calculateScore(Memory{Importance: 0, LastAccessed: c.lastAccessed}, map[string]bool{})
			if math.Abs(got-c.want) > scoreTolerance {
				t.Errorf("recency bonus for a memory last accessed %v ago: got %v, want %v",
					time.Since(c.lastAccessed).Round(time.Minute), got, c.want)
			}
		})
	}
}

// TestBuildContextRanksAcrossTheOneDayRecencyCut holds the call site.
//
// The test above pins calculateScore; it cannot say that BuildContext still
// asks it. The two memories here carry the same importance, the same token
// count and no tags, so the recency bucket is the only thing that separates
// them — and the fresher one is deliberately given the LATER id, so the
// id tie-break the comparator falls back on would produce the opposite order.
// A passing run therefore means the bonus decided it.
func TestBuildContextRanksAcrossTheOneDayRecencyCut(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "recency-cut.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	now := time.Now()
	rows := []struct {
		id           string
		lastAccessed time.Time
	}{
		{"a-two-hours-past-the-cut", now.Add(-26 * time.Hour)},
		{"b-two-hours-inside-the-cut", now.Add(-22 * time.Hour)},
	}
	for _, r := range rows {
		if err := s.Save(Memory{
			ID:           r.id,
			Content:      "content of " + r.id,
			Importance:   0.5,
			Tokens:       100,
			LastAccessed: r.lastAccessed,
			CreatedAt:    r.lastAccessed,
			Source:       "test",
		}); err != nil {
			t.Fatalf("save %s: %v", r.id, err)
		}
	}

	got, _, err := s.BuildContext(BuildContextRequest{TokenBudget: 32000, IncludeAlwaysLoad: true})
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the fixture no longer straddles the one-day cut: BuildContext returned %d memories, want 2", len(got))
	}

	want := []string{"b-two-hours-inside-the-cut", "a-two-hours-past-the-cut"}
	if !equalIDs(idsOf(got), want) {
		t.Fatalf("the one-day recency bonus did not decide the order\n  got  %v\n  want %v (ascending id order would be the reverse)",
			idsOf(got), want)
	}
}

// TestDecayImportanceStraddlesTheOneDayCut pins the third authoring of the
// one-day line, and the factor it applies when it fires.
//
// The pre-existing TestDecayImportance has one row, two days old, and asserts
// only that importance came out lower than it went in. One row on one side of
// the cut says nothing about where the cut is, and a direction says nothing
// about the size of the step.
//
// What that test does notice, it notices by accident. It reads the row back
// through Get, which multiplies importance by 1.01 on the way out, so a decay
// is visible through that reader only while the factor stays under 1/1.01 =
// 0.990099 — and 0.99 clears it by a ten-thousandth. Weaken the decay to 0.995
// and the stored value still falls while that test goes red; strengthen it to
// 0.98 and the test cannot tell. Neither response is the one it claims to make.
//
// This test reads through ListRecent, which does not bump, and asserts values.
func TestDecayImportanceStraddlesTheOneDayCut(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "decay-cut.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	const startingImportance = 0.8
	now := time.Now()
	rows := []struct {
		id           string
		lastAccessed time.Time
		want         float64
	}{
		{"an-hour-inside-the-cut", now.Add(-23 * time.Hour), startingImportance},
		{"an-hour-outside-the-cut", now.Add(-25 * time.Hour), startingImportance * 0.99},
	}
	for _, r := range rows {
		if err := s.Save(Memory{
			ID:           r.id,
			Content:      "content of " + r.id,
			Importance:   startingImportance,
			Tokens:       100,
			LastAccessed: r.lastAccessed,
			CreatedAt:    r.lastAccessed,
			Source:       "test",
		}); err != nil {
			t.Fatalf("save %s: %v", r.id, err)
		}
	}

	if err := s.DecayImportance(); err != nil {
		t.Fatalf("DecayImportance: %v", err)
	}

	memories, err := s.ListRecent(100, 0)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	byID := make(map[string]Memory, len(memories))
	for _, m := range memories {
		byID[m.ID] = m
	}
	if len(byID) != len(rows) {
		t.Fatalf("the fixture no longer straddles the one-day cut: %d memories came back, want %d", len(byID), len(rows))
	}

	for _, r := range rows {
		m := byID[r.id]
		if math.Abs(m.Importance-r.want) > scoreTolerance {
			t.Errorf("importance of %s (last accessed %v ago): got %v, want %v",
				r.id, time.Since(r.lastAccessed).Round(time.Hour), m.Importance, r.want)
		}
	}
}

// recentFileFixture writes one file per age and runs PrepareSession over them,
// returning the saved memories by id.
//
// Each age is offset by an extra thirty seconds so that the integer minute the
// code computes is unambiguous: `int(time.Since(modTime).Minutes())` truncates,
// so a file written exactly 60 minutes back lands on 60 or 59 depending on how
// long the scan takes to reach it. Half a minute of slack is far more than the
// walk needs and keeps every row on the side of the cut it was written for.
func recentFileFixture(t *testing.T, agesByName map[string]time.Duration) map[string]Memory {
	t.Helper()

	rootDir := t.TempDir()
	start := time.Now()
	for name, age := range agesByName {
		path := filepath.Join(rootDir, name)
		if err := os.WriteFile(path, []byte("test content"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		modTime := start.Add(-age - 30*time.Second)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}

	s, err := NewStore(filepath.Join(t.TempDir(), "recent-files.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfg := PrepareSessionConfig{
		RootDir:        rootDir,
		IdentityText:   "I am a test agent",
		AgentName:      "test-agent",
		RecencyWindow:  24 * time.Hour,
		RecentFilesTTL: 10 * time.Minute,
	}
	if err := s.PrepareSession(context.Background(), cfg); err != nil {
		t.Fatalf("PrepareSession: %v", err)
	}

	// ListRecent, not Get: Get calls updateAccess, which multiplies importance
	// by 1.01 on the way out. Reading the bucketed importance through it would
	// compare 0.707 against 0.7 and the test would be asserting the reader.
	memories, err := s.ListRecent(100, 0)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	byID := make(map[string]Memory, len(memories))
	for _, m := range memories {
		byID[m.ID] = m
	}
	return byID
}

// TestRecentFileAgeBucketsStraddleTheirCuts pins the one-hour and six-hour cuts
// in loadRecentFiles, the three importances they select between, and the age
// string each bucket writes.
func TestRecentFileAgeBucketsStraddleTheirCuts(t *testing.T) {
	cases := []struct {
		name           string
		age            time.Duration
		wantImportance float64
		wantAge        string
	}{
		{"just-inside-one-hour.txt", 59 * time.Minute, 0.7, "59 minutes ago"},
		{"just-outside-one-hour.txt", 60 * time.Minute, 0.6, "1 hour ago"},
		{"just-inside-six-hours.txt", 359 * time.Minute, 0.6, "5 hours ago"},
		{"just-outside-six-hours.txt", 360 * time.Minute, 0.5, "6 hours ago"},
	}

	agesByName := make(map[string]time.Duration, len(cases))
	for _, c := range cases {
		agesByName[c.name] = c.age
	}
	byID := recentFileFixture(t, agesByName)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, ok := byID["recent:"+c.name]
			if !ok {
				t.Fatalf("the fixture no longer straddles the age cuts: no memory was saved for %s", c.name)
			}
			if math.Abs(m.Importance-c.wantImportance) > scoreTolerance {
				t.Errorf("importance for a file modified %v ago: got %v, want %v",
					c.age, m.Importance, c.wantImportance)
			}
			wantContent := fmt.Sprintf("Recently modified (%s): %s", c.wantAge, c.name)
			if m.Content != wantContent {
				t.Errorf("age string for a file modified %v ago:\n  got  %q\n  want %q",
					c.age, m.Content, wantContent)
			}
		})
	}
}
