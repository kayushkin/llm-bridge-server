package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/modelstoreroles"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge/msg"
)

// schedulerCallsThisServer is this server as the scheduler inside it calls
// it: the instances an agent job's session starts on, the session core that
// creates the session and sends it its turn, the session the job follows,
// model-store's roles, and the providers each harness runs. Each call does
// what the route that answered it before does (GET /instances,
// POST /sessions, POST /sessions/{id}/send, GET /sessions/{id},
// GET /models/api/roles and …/roles/{role}, GET /harnesses), as the internal
// service acting as nobody, which none of those routes narrowed.
type schedulerCallsThisServer struct {
	server *Server
}

var (
	_ executor.Bridge              = schedulerCallsThisServer{}
	_ sessionmodelrole.RoleSources = schedulerCallsThisServer{}
	_ modelstoreroles.Roles        = schedulerCallsThisServer{}
)

// SchedulerBridge is this server as the scheduler's agent jobs call it.
func (s *Server) SchedulerBridge() executor.Bridge { return schedulerCallsThisServer{server: s} }

// SchedulerModelRoleSources is model-store and this server's harnesses as
// the scheduler checks the role an agent job's session starts on.
func (s *Server) SchedulerModelRoleSources() sessionmodelrole.RoleSources {
	return schedulerCallsThisServer{server: s}
}

// SchedulerModelStoreRoles is model-store's roles as the scheduler checks a
// model-role setting.
func (s *Server) SchedulerModelStoreRoles() modelstoreroles.Roles {
	return schedulerCallsThisServer{server: s}
}

func (calls schedulerCallsThisServer) Instances() ([]msg.Instance, error) {
	return calls.server.machineRoutes.ListInstancesWithMachines()
}

// CreateSession creates a session through the session core POST /sessions
// calls, so a job's session is checked and created as a person's is.
func (calls schedulerCallsThisServer) CreateSession(ctx context.Context, request msg.CreateSessionRequest) (*msg.ManagedSession, error) {
	created, refusal := calls.server.sessions.CreateSessionFromThisServer(ctx, request)
	if created == nil {
		return nil, fmt.Errorf("POST /sessions answered %d: %s", refusal.Status, strings.TrimSpace(string(refusal.Body)))
	}
	return created, nil
}

// SendMessage sends a message through the session core POST
// /sessions/{id}/send calls, from no sender principal: the scheduler sends it,
// not a person.
func (calls schedulerCallsThisServer) SendMessage(ctx context.Context, sessionID string, request msg.SendMessageRequest) error {
	answer := calls.server.sessions.SendMessageFromThisServer(ctx, sessionID, request, "")
	if !answer.Sent() {
		return fmt.Errorf("POST /sessions/%s/send answered %d: %s", sessionID, answer.Status, strings.TrimSpace(string(answer.Body)))
	}
	return nil
}

func (calls schedulerCallsThisServer) Session(sessionID string) (*msg.ManagedSession, error) {
	session, err := calls.server.store.GetSession(sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %s: %w", sessionID, executor.ErrSessionNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading session %s: %w", sessionID, err)
	}
	return session, nil
}

// errModelStoreOff is a read of model-store on a server built without one.
var errModelStoreOff = errors.New("this server was built without a model registry")

func (calls schedulerCallsThisServer) FirstModelOfRole(role string) (sessionmodelrole.Model, error) {
	if calls.server.modelStore == nil {
		return sessionmodelrole.Model{}, fmt.Errorf("resolve model role %q with model-store: %w", role, errModelStoreOff)
	}
	model, err := calls.server.modelStore.ResolveRole(role)
	if err != nil {
		return sessionmodelrole.Model{}, fmt.Errorf("resolve model role %q with model-store: %w", role, err)
	}
	return sessionmodelrole.Model{ID: model.ID, Provider: model.Provider}, nil
}

func (calls schedulerCallsThisServer) ProvidersOfHarness(harness msg.Harness) ([]string, error) {
	for _, described := range calls.server.harnessCatalog.DiscoverHarnesses() {
		if described.Name == string(harness) {
			return described.SupportedProviders, nil
		}
	}
	return nil, fmt.Errorf("this server does not know harness %q", harness)
}

func (calls schedulerCallsThisServer) CanonicalRoleNames() []string {
	return models.CanonicalRoles
}

func (calls schedulerCallsThisServer) RoleModelLists() (map[string][]string, error) {
	if calls.server.modelStore == nil {
		return nil, errModelStoreOff
	}
	return calls.server.modelStore.RoleModelLists()
}
