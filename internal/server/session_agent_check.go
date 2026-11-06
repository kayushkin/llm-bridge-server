package server

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// errUnknownAgent is a session agent_id that agent-store has no agent for.
var errUnknownAgent = errors.New("unknown agent")

// errAgentStoreNotConfigured is a session agent_id on a server with no
// agent-store to check it with.
var errAgentStoreNotConfigured = errors.New("this server has no agent-store to check agent_id with")

// checkAgentExists confirms that a session's agent_id names an agent
// agent-store has: its numeric id, or its slug, which is what sessions send
// today. Until 2026-11-05 an agent_id was checked only when the principal's
// can_run_as grants restricted it, so a principal with none started a session
// as an agent nobody had, and nothing said so.
func (s *Server) checkAgentExists(agentID string) error {
	if s.agentStore == nil {
		return errAgentStoreNotConfigured
	}
	var err error
	if id, parseErr := strconv.ParseInt(agentID, 10, 64); parseErr == nil {
		_, err = s.agentStore.GetAgent(id)
	} else {
		_, err = s.agentStore.GetAgentBySlug(agentID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: agent_id %q is neither the id nor the slug of an agent in agent-store", errUnknownAgent, agentID)
	}
	if err != nil {
		return fmt.Errorf("could not read agent %q from agent-store: %w", agentID, err)
	}
	return nil
}
