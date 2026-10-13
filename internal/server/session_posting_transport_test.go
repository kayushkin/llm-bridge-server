package server

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// A runner or ssh instance cannot deliver environment variables, and
// StartOnInstance refuses a session that carries any. The posting variables are
// an extra, so they are left off there — before this, every session carried
// them and none could start on a runner machine.
func TestSessionPostingEnvironmentIsLocalOnly(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	session := newSessionForSignals(t, st, "br_posting_transport", msg.SessionTypeInteractive)

	local := &msg.Instance{ID: "inst-local", Machine: &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal}}
	if got := srv.sessionPostingEnvironmentForInstance(session, local); len(got) == 0 {
		t.Fatal("a local instance got no posting variables")
	}
	for _, transport := range []msg.Transport{msg.TransportRunner, msg.TransportSSH} {
		remote := &msg.Instance{ID: "inst-remote", Machine: &msg.Machine{ID: "m_remote", Transport: transport}}
		if got := srv.sessionPostingEnvironmentForInstance(session, remote); len(got) != 0 {
			t.Errorf("%s instance got %d posting variables; StartOnInstance would refuse the session", transport, len(got))
		}
	}
	if got := srv.sessionPostingEnvironmentForInstance(session, &msg.Instance{ID: "inst-no-machine"}); len(got) != 0 {
		t.Errorf("an instance with no machine got %d posting variables", len(got))
	}
}
