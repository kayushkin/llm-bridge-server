package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// ErrNotGranted is a session that names a principal whose grants do not
// allow where or as whom it would run. The caller's to fix, by granting or by
// choosing differently; the message names what the grants do allow.
var ErrNotGranted = errors.New("not granted")

// checkPrincipalMayRunHere is the can_dispatch_on and can_run_as gate: a
// session started as a principal may start only on an instance (or a
// machine) the principal's grants name, and only as an agent they name.
//
// What the grants allow is decided in principal_grants.go: lenient by the
// operator's choice on 2026-09-11 (a principal holding NO grant of a relation
// is not restricted by it, so a default principal set before any grant is
// written does not lock every session out), except for a kind principal-store
// marks restricted to its grants, such as a peer; and a grant of "*" names
// every resource of its type. A grant-store that cannot answer is an error: a
// session that named a principal must not start because the check failed.
//
// Called twice on purpose: at POST /sessions, so the caller gets a 403 with
// the reason, and at spawn, so a session created before its principal's
// grants changed is judged against the grants as they are now.
//
// The session's agent_id is, today, agent-store's slug rather than its id —
// every live row that carries one carries a slug, and the harness takes it
// verbatim. Grants name agent-store's numeric id, so the slug is resolved
// through the in-process agent-store here; a numeric agent_id is taken as the
// id directly.
//
// subject is whose grants decide: the session's principal, and the peer it
// was started through for one of that peer's users (grantSubjectOfSession).
func (s *Server) checkPrincipalMayRunHere(ctx context.Context, subject grantSubject, sess *store.Session, inst *msg.Instance) error {
	if sess.PrincipalID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dispatch, err := s.dispatchGrantsOf(ctx, subject)
	if err != nil {
		return err
	}
	if !dispatch.permits(inst.ID, inst.MachineID) {
		return fmt.Errorf("%w: %s may not start a session on instance %s (machine %s); %s",
			ErrNotGranted, subject, inst.ID, inst.MachineID, dispatch.describe())
	}
	if sess.AgentID == "" {
		return nil
	}
	runAs, err := s.grantedResourcesOf(ctx, subject, "can_run_as", "agent")
	if err != nil {
		return err
	}
	if !runAs.restricts() || runAs.namesAll() {
		return nil
	}
	agentID, err := s.resolveAgentID(sess.AgentID)
	if err != nil {
		return fmt.Errorf("%w: %s, and the session's agent %q could not be resolved to an agent-store id: %v",
			ErrNotGranted, runAs.describe(), sess.AgentID, err)
	}
	if !runAs.permits(agentID) {
		return fmt.Errorf("%w: %s may not start a session as agent %s (agent-store id %s); %s",
			ErrNotGranted, subject, sess.AgentID, agentID, runAs.describe())
	}
	return nil
}

// grantSubjectOfSession is whose grants decide for a stored session: its
// principal, and the peer it was started through when a connected server
// started it for one of its users (store.SessionPeerOrigin). A session that
// does not exist yet, as at create, has no origin row; the caller passes the
// request's subject instead.
func (s *Server) grantSubjectOfSession(sess *store.Session) (grantSubject, error) {
	subject := grantSubject{principalID: sess.PrincipalID}
	// A server built with no session store (some tests) has no origins.
	if sess.SessionID == "" || sess.PrincipalID == "" || s.store == nil {
		return subject, nil
	}
	origin, found, err := s.store.SessionPeerOriginOfLineage(sess.SessionID)
	if err != nil {
		return grantSubject{}, fmt.Errorf("read session %s's peer origin: %w", sess.SessionID, err)
	}
	if found {
		subject.throughPeerPrincipalID = origin.PeerPrincipalID
	}
	return subject, nil
}

// resolveAgentID turns a session's agent_id — a slug today, or already an id
// — into agent-store's numeric id as a string, which is what a grant names.
func (s *Server) resolveAgentID(agentID string) (string, error) {
	if _, err := strconv.ParseInt(agentID, 10, 64); err == nil {
		return agentID, nil
	}
	if s.agentStore == nil {
		return "", errors.New("no agent-store is configured to resolve the slug")
	}
	agent, err := s.agentStore.GetAgentBySlug(strings.TrimSpace(agentID))
	if err != nil {
		return "", fmt.Errorf("agent-store has no agent with slug %q: %w", agentID, err)
	}
	return strconv.FormatInt(agent.ID, 10), nil
}

// grantGateStatus maps a gate failure to the HTTP answer POST /sessions gives.
func grantGateStatus(err error) (int, string) {
	if errors.Is(err, ErrNotGranted) {
		return 403, "not_granted"
	}
	if errors.Is(err, grantclient.ErrPrincipalUnknown) {
		return 400, "unknown_principal"
	}
	return 502, "grant_store_unavailable"
}
