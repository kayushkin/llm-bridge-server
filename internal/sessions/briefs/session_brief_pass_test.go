package briefs

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestTheBriefPassSkipsWhatIsNotASettledChatTurn(t *testing.T) {
	cases := []struct {
		name  string
		sess  *store.Session
		state msg.SessionState
		skip  bool
	}{
		{"chat, idle", &store.Session{Type: msg.SessionTypeInteractive}, msg.SessionIdle, false},
		{"chat, awaiting the person", &store.Session{Type: msg.SessionTypeInteractive}, msg.SessionAwaitingUser, false},
		{"autonomous worker", &store.Session{Type: msg.SessionTypeAutonomous}, msg.SessionIdle, true},
		{"session with no type", &store.Session{}, msg.SessionIdle, true},
		{"renamer", &store.Session{Type: msg.SessionTypeInteractive, Purpose: store.RenamerSessionPurpose}, msg.SessionIdle, true},
		{"errored turn", &store.Session{Type: msg.SessionTypeInteractive}, msg.SessionError, true},
	}
	for _, c := range cases {
		if got := skipSessionBriefReason(c.sess, c.state) != ""; got != c.skip {
			t.Errorf("%s: skipped = %v, want %v", c.name, got, c.skip)
		}
	}
}

func TestKeepHeadAndTailKeepsBothEnds(t *testing.T) {
	text := strings.Repeat("a", 50) + strings.Repeat("m", 1000) + strings.Repeat("z", 50)
	got := keepHeadAndTail(text, 200)
	if !strings.HasPrefix(got, strings.Repeat("a", 50)) || !strings.HasSuffix(got, strings.Repeat("z", 50)) {
		t.Errorf("lost an end: %q", got)
	}
}
