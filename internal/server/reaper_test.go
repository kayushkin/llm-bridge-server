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
