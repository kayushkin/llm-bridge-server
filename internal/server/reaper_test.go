package server

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestReapDecision(t *testing.T) {
	const (
		unattendedTimeout = 15 * time.Minute
		attendedTimeout   = 60 * time.Minute
	)
	now := time.Date(2026, 5, 29, 18, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name         string
		mode         msg.SessionMode
		sessionType  msg.SessionType
		state        msg.SessionState
		lastAct      time.Time
		updatedAt    time.Time
		unattendedTO time.Duration
		attendedTO   time.Duration
		wantReap     bool
	}{
		{
			name:         "unattended idle past timeout is reaped",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionIdle,
			lastAct:      ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
		{
			// The leak this reaper exists for: an autoworker fire finishes its
			// turn, drops to idle, and the claude process sits holding ~300MB
			// waiting for a second turn that never comes.
			name:         "autoworker left idle is reaped",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionIdle,
			lastAct:      ago(4 * time.Hour),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
		{
			// Regression: this previously asserted wantReap=true, which meant a
			// dash chat the user walked away from was killed after 15 minutes
			// and the reply they came back to send had nowhere to land.
			name:         "awaiting_user is never reaped — a human is deliberating",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionAwaitingUser,
			lastAct:      ago(10 * time.Hour),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			// Reaping this would cancel the parked hook and auto-deny a
			// permission prompt the user never got to answer.
			name:         "awaiting_permission is never reaped",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionAwaitingPermission,
			lastAct:      ago(10 * time.Hour),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "blocked-on-user beats even a disabled-looking config",
			mode:         msg.SessionModePTY,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionWaitingApproval,
			lastAct:      ago(10 * time.Hour),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			// The dash chat: events mode, but a human is sitting in front of it,
			// so it gets the attended timeout rather than the 15m one.
			name:         "interactive events-mode within attended timeout is kept",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionIdle,
			lastAct:      ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "interactive events-mode past attended timeout is reaped",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionIdle,
			lastAct:      ago(70 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
		{
			name:         "system sessions are unattended — reaped on the short timeout",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeSystem,
			state:        msg.SessionIdle,
			lastAct:      ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
		{
			name:         "unattended idle within timeout is kept",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionIdle,
			lastAct:      ago(5 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "tool_running is never reaped even when stale",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionToolRunning,
			lastAct:      ago(45 * time.Minute), // long bash tool, no events
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "model_generating is never reaped",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionModelGenerating,
			lastAct:      ago(30 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "rate_limited (self-healing) is never reaped",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionRateLimited,
			lastAct:      ago(30 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "pty within attended timeout is kept (human reading)",
			mode:         msg.SessionModePTY,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionRunning, // pty state never leaves running
			lastAct:      ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "pty past attended timeout is reaped despite running state",
			mode:         msg.SessionModePTY,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionRunning,
			lastAct:      ago(70 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
		{
			// A pty session is attended by virtue of its I/O contract even when
			// the caller left Type unset — the human is on the far end of the fd.
			name:         "pty with no session type still gets the attended timeout",
			mode:         msg.SessionModePTY,
			sessionType:  msg.SessionType(""),
			state:        msg.SessionRunning,
			lastAct:      ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "no events yet falls back to updatedAt — fresh is kept",
			mode:         msg.SessionModePTY,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionStarting,
			lastAct:      time.Time{}, // zero: no event has landed
			updatedAt:    ago(1 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			// The healthcheck orphans: llm-bridge-server created the session but
			// the caller never sent a prompt, so no event ever landed and the
			// process idled at ~300MB forever. updatedAt is the only clock here,
			// which is why a reap-on-first-result policy would never catch these.
			name:         "session that never got a prompt is reaped on updatedAt",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionIdle,
			lastAct:      time.Time{},
			updatedAt:    ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
		{
			name:         "unattended reaping disabled (timeout 0) keeps everything",
			mode:         msg.SessionModeEvents,
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionIdle,
			lastAct:      ago(10 * time.Hour),
			unattendedTO: 0,
			attendedTO:   attendedTimeout,
			wantReap:     false,
		},
		{
			name:         "attended reaping disabled (timeout 0) keeps everything",
			mode:         msg.SessionModePTY,
			sessionType:  msg.SessionTypeInteractive,
			state:        msg.SessionRunning,
			lastAct:      ago(10 * time.Hour),
			unattendedTO: unattendedTimeout,
			attendedTO:   0,
			wantReap:     false,
		},
		{
			name:         "empty mode on an autonomous session is treated as events",
			mode:         msg.SessionMode(""),
			sessionType:  msg.SessionTypeAutonomous,
			state:        msg.SessionIdle,
			lastAct:      ago(20 * time.Minute),
			unattendedTO: unattendedTimeout,
			attendedTO:   attendedTimeout,
			wantReap:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, reap := reapDecision(now, tt.mode, tt.sessionType, tt.state, tt.lastAct, tt.updatedAt, tt.unattendedTO, tt.attendedTO)
			if reap != tt.wantReap {
				t.Errorf("reapDecision() = %v, want %v", reap, tt.wantReap)
			}
		})
	}
}
