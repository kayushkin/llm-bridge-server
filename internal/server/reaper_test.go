package server

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestReapDecision(t *testing.T) {
	const (
		idleTimeout = 15 * time.Minute
		ptyTimeout  = 60 * time.Minute
	)
	now := time.Date(2026, 5, 29, 18, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name      string
		mode      msg.SessionMode
		state     msg.SessionState
		lastAct   time.Time
		updatedAt time.Time
		idleTO    time.Duration
		ptyTO     time.Duration
		wantReap  bool
	}{
		{
			name:     "events idle past timeout is reaped",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionIdle,
			lastAct:  ago(20 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: true,
		},
		{
			name:     "events awaiting_user past timeout is reaped",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionAwaitingUser,
			lastAct:  ago(16 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: true,
		},
		{
			name:     "events idle within timeout is kept",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionIdle,
			lastAct:  ago(5 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:     "events tool_running is never reaped even when stale",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionToolRunning,
			lastAct:  ago(45 * time.Minute), // long bash tool, no events
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:     "events model_generating is never reaped",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionModelGenerating,
			lastAct:  ago(30 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:     "events rate_limited (self-healing) is never reaped",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionRateLimited,
			lastAct:  ago(30 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:     "pty within longer timeout is kept (human reading)",
			mode:     msg.SessionModePTY,
			state:    msg.SessionRunning, // pty state never leaves running
			lastAct:  ago(20 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:     "pty past longer timeout is reaped despite running state",
			mode:     msg.SessionModePTY,
			state:    msg.SessionRunning,
			lastAct:  ago(70 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: true,
		},
		{
			name:      "no events yet falls back to updatedAt — fresh is kept",
			mode:      msg.SessionModePTY,
			state:     msg.SessionStarting,
			lastAct:   time.Time{}, // zero: no event has landed
			updatedAt: ago(1 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  false,
		},
		{
			name:      "no events yet but updatedAt is old — reaped",
			mode:      msg.SessionModeEvents,
			state:     msg.SessionIdle,
			lastAct:   time.Time{},
			updatedAt: ago(20 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  true,
		},
		{
			name:     "events reaping disabled (timeout 0) keeps everything",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionIdle,
			lastAct:  ago(10 * time.Hour),
			idleTO:   0,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:     "pty reaping disabled (timeout 0) keeps everything",
			mode:     msg.SessionModePTY,
			state:    msg.SessionRunning,
			lastAct:  ago(10 * time.Hour),
			idleTO:   idleTimeout,
			ptyTO:    0,
			wantReap: false,
		},
		{
			// The bug this case exists for: `starting` was in the
			// never-reap set, so a harness that spawned and never emitted
			// a first event left the session parked here forever with Send
			// disabled behind it. Measured 2026-09-02 on
			// br_1788301063417373298, which sat in `starting` for 10+ min
			// with no event of any kind after the resume.
			name:      "events starting past timeout with no first event is reaped",
			mode:      msg.SessionModeEvents,
			state:     msg.SessionStarting,
			lastAct:   time.Time{}, // the harness never spoke
			updatedAt: ago(20 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  true,
		},
		{
			name:      "events starting within timeout is kept (loading a big transcript)",
			mode:      msg.SessionModeEvents,
			state:     msg.SessionStarting,
			lastAct:   time.Time{},
			updatedAt: ago(2 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  false,
		},
		{
			// The trap in the repair. On a RESUME the events table still
			// holds the previous process's turns, so lastActivity is hours
			// stale the instant the new process spawns. Measuring `starting`
			// from it would reap the fresh process on the very first tick —
			// trading an eternal `starting` for an instant kill. The clock
			// must run from updatedAt, the spawn stamp.
			name:      "resumed session starting is measured from spawn, not the previous life's last event",
			mode:      msg.SessionModeEvents,
			state:     msg.SessionStarting,
			lastAct:   ago(5 * time.Hour), // last turn_complete of the session being resumed
			updatedAt: ago(1 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  false,
		},
		{
			// Same reference rule, the other side of the cutoff: a resume
			// that wedges is still reaped once the spawn itself is old.
			name:      "resumed session wedged in starting past timeout is reaped",
			mode:      msg.SessionModeEvents,
			state:     msg.SessionStarting,
			lastAct:   ago(5 * time.Hour),
			updatedAt: ago(20 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  true,
		},
		{
			// `starting` is carved out of the active-state exemption by
			// name, not by widening it — every state the HARNESS reports
			// must still be exempt however stale it looks.
			name:     "events running is still never reaped",
			mode:     msg.SessionModeEvents,
			state:    msg.SessionRunning,
			lastAct:  ago(45 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: false,
		},
		{
			name:      "pty starting is governed by the pty timeout, not reaped early",
			mode:      msg.SessionModePTY,
			state:     msg.SessionStarting,
			lastAct:   time.Time{},
			updatedAt: ago(20 * time.Minute),
			idleTO:    idleTimeout,
			ptyTO:     ptyTimeout,
			wantReap:  false,
		},
		{
			name:      "starting with reaping disabled is kept",
			mode:      msg.SessionModeEvents,
			state:     msg.SessionStarting,
			lastAct:   time.Time{},
			updatedAt: ago(10 * time.Hour),
			idleTO:    0,
			ptyTO:     ptyTimeout,
			wantReap:  false,
		},
		{
			name:     "empty mode is treated as events",
			mode:     msg.SessionMode(""),
			state:    msg.SessionIdle,
			lastAct:  ago(20 * time.Minute),
			idleTO:   idleTimeout,
			ptyTO:    ptyTimeout,
			wantReap: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, reap := reapDecision(now, tt.mode, tt.state, tt.lastAct, tt.updatedAt, tt.idleTO, tt.ptyTO)
			if reap != tt.wantReap {
				t.Errorf("reapDecision() = %v, want %v", reap, tt.wantReap)
			}
		})
	}
}
