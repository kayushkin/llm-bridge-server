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
// Lenient in the same shape as the tool filter, by the operator's choice on
// 2026-09-11: a principal holding NO grant of a relation is not restricted
// by it — so a default principal set before any grant is written does not
// lock every session out — and a principal holding any grant of it is
// refused outside them. A grant-store that cannot answer is an error: a
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
func (s *Server) checkPrincipalMayRunHere(ctx context.Context, sess *store.Session, inst *msg.Instance) error {
	if sess.PrincipalID == "" {
		return nil
	}
	if s.grantClient == nil {
		return fmt.Errorf("session %s is started as %s but this server has no grant-store to read its grants from (LLMBRIDGE_GRANT_STORE_URL)", sess.SessionID, sess.PrincipalID)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	instanceIDs, err := s.grantClient.EffectiveResourceIDs(ctx, sess.PrincipalID, "can_dispatch_on", "instance")
	if err != nil {
		return fmt.Errorf("could not read %s's can_dispatch_on grants: %w", sess.PrincipalID, err)
	}
	machineIDs, err := s.grantClient.EffectiveResourceIDs(ctx, sess.PrincipalID, "can_dispatch_on", "machine")
	if err != nil {
		return fmt.Errorf("could not read %s's can_dispatch_on grants: %w", sess.PrincipalID, err)
	}
	if len(instanceIDs)+len(machineIDs) > 0 && !contains(instanceIDs, inst.ID) && !contains(machineIDs, inst.MachineID) {
		return fmt.Errorf("%w: %s may not start a session on instance %s (machine %s); their can_dispatch_on grants name instances %v and machines %v",
			ErrNotGranted, sess.PrincipalID, inst.ID, inst.MachineID, instanceIDs, machineIDs)
	}

	if sess.AgentID == "" {
		return nil
	}
	agentIDs, err := s.grantClient.EffectiveResourceIDs(ctx, sess.PrincipalID, "can_run_as", "agent")
	if err != nil {
		return fmt.Errorf("could not read %s's can_run_as grants: %w", sess.PrincipalID, err)
	}
	if len(agentIDs) == 0 {
		return nil
	}
	agentID, err := s.resolveAgentID(sess.AgentID)
	if err != nil {
		return fmt.Errorf("%w: %s holds can_run_as grants for agents %v, and the session's agent %q could not be resolved to an agent-store id: %v",
			ErrNotGranted, sess.PrincipalID, agentIDs, sess.AgentID, err)
	}
	if !contains(agentIDs, agentID) {
		return fmt.Errorf("%w: %s may not start a session as agent %s (agent-store id %s); their can_run_as grants name agents %v",
			ErrNotGranted, sess.PrincipalID, sess.AgentID, agentID, agentIDs)
	}
	return nil
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

func contains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
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
