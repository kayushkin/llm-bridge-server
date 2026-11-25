package rulestore

import (
	"sync"
	"time"
)

// The reactor's recent activity, for a person watching it work.
//
// Runs are the durable record of what rules decided. This is the rest: the
// feed connecting and dropping, every full read of the sessions, every sweep
// with what it checked and how long it took, every error. It is kept in memory
// and lost on restart on purpose — it answers "is it working right now", and
// the journal (`journalctl --user -u rule-store`) keeps the same lines longer.

// Activity kinds.
const (
	ActivityFeed  = "feed"
	ActivitySeed  = "seed"
	ActivitySweep = "sweep"
	ActivityRun   = "run"
	ActivityError = "error"
)

// ActivityKinds is every kind an activity entry can have.
var ActivityKinds = []string{ActivityFeed, ActivitySeed, ActivitySweep, ActivityRun, ActivityError}

// activityCapacity is how many entries the log keeps. At one sweep a minute
// it holds several hours even when nothing else happens.
const activityCapacity = 1000

// Activity is one entry.
type Activity struct {
	Seq     uint64 `json:"seq"`
	At      int64  `json:"at"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	RuleID  string `json:"rule_id,omitempty"`
	RunID   string `json:"run_id,omitempty"`
	Subject string `json:"subject_id,omitempty"`
}

type activityLog struct {
	mu      sync.Mutex
	next    uint64
	entries []Activity
}

func (l *activityLog) add(entry Activity) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	entry.Seq = l.next
	if entry.At == 0 {
		entry.At = time.Now().Unix()
	}
	l.entries = append(l.entries, entry)
	if len(l.entries) > activityCapacity {
		l.entries = l.entries[len(l.entries)-activityCapacity:]
	}
}

// since returns entries after seq, newest first, at most limit.
func (l *activityLog) since(seq uint64, kind string, limit int) []Activity {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Activity{}
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		entry := l.entries[i]
		if entry.Seq <= seq {
			break
		}
		if kind != "" && entry.Kind != kind {
			continue
		}
		out = append(out, entry)
	}
	return out
}
