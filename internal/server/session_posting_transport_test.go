package server

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// An ssh instance cannot deliver environment variables, and StartOnInstance
// refuses a session that carries any there, so the posting variables are left
// off. A runner delivers them through RunnerSpawn.Env (since 2026-10-14), so a
// session on a runner machine can share files and offer actions too.
func TestSessionPostingEnvironmentReachesLocalAndRunnerChildrenNotSSH(t *testing.T) {
	srv, st, _ := testServerForSessionActions(t)
	session := newSessionForSignals(t, st, "br_posting_transport", msg.SessionTypeInteractive)

	local := &msg.Instance{ID: "inst-local", Machine: &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal}}
	if got := srv.sessionPostingEnvironmentForInstance(session, local); len(got) == 0 {
		t.Fatal("a local instance got no posting variables")
	}
	runner := &msg.Instance{ID: "inst-runner", Machine: &msg.Machine{ID: "m_runner", Transport: msg.TransportRunner}}
	if got := srv.sessionPostingEnvironmentForInstance(session, runner); len(got) == 0 {
		t.Error("a runner instance got no posting variables")
	}
	ssh := &msg.Instance{ID: "inst-ssh", Machine: &msg.Machine{ID: "m_ssh", Transport: msg.TransportSSH}}
	if got := srv.sessionPostingEnvironmentForInstance(session, ssh); len(got) != 0 {
		t.Errorf("an ssh instance got %d posting variables; StartOnInstance would refuse the session", len(got))
	}
	if got := srv.sessionPostingEnvironmentForInstance(session, &msg.Instance{ID: "inst-no-machine"}); len(got) != 0 {
		t.Errorf("an instance with no machine got %d posting variables", len(got))
	}
}
