package server

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/modelstoreroles"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge/msg"
)

// schedulerReadsThisServer is this server as the scheduler inside it reads
// it: the instances and sessions an agent job follows, model-store's roles,
// and the providers each harness runs. Each read calls what the route that
// answered it before calls (GET /instances, GET /sessions/{id},
// GET /models/api/roles and …/roles/{role}, GET /harnesses), as the
// internal service, which none of those routes narrowed.
type schedulerReadsThisServer struct {
	server *Server
}

var (
	_ executor.Bridge              = schedulerReadsThisServer{}
	_ sessionmodelrole.RoleSources = schedulerReadsThisServer{}
	_ modelstoreroles.Roles        = schedulerReadsThisServer{}
)

// SchedulerBridge is this server as the scheduler's agent jobs read it.
func (s *Server) SchedulerBridge() executor.Bridge { return schedulerReadsThisServer{server: s} }

// SchedulerModelRoleSources is model-store and this server's harnesses as
// the scheduler checks the role an agent job's session starts on.
func (s *Server) SchedulerModelRoleSources() sessionmodelrole.RoleSources {
	return schedulerReadsThisServer{server: s}
}

// SchedulerModelStoreRoles is model-store's roles as the scheduler checks a
// model-role setting.
func (s *Server) SchedulerModelStoreRoles() modelstoreroles.Roles {
	return schedulerReadsThisServer{server: s}
}

func (reads schedulerReadsThisServer) Instances() ([]msg.Instance, error) {
	return reads.server.listInstancesWithMachines()
}

func (reads schedulerReadsThisServer) Session(sessionID string) (*msg.ManagedSession, error) {
	session, err := reads.server.store.GetSession(sessionID)
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

func (reads schedulerReadsThisServer) FirstModelOfRole(role string) (sessionmodelrole.Model, error) {
	if reads.server.modelStore == nil {
		return sessionmodelrole.Model{}, fmt.Errorf("resolve model role %q with model-store: %w", role, errModelStoreOff)
	}
	model, err := reads.server.modelStore.ResolveRole(role)
	if err != nil {
		return sessionmodelrole.Model{}, fmt.Errorf("resolve model role %q with model-store: %w", role, err)
	}
	return sessionmodelrole.Model{ID: model.ID, Provider: model.Provider}, nil
}

func (reads schedulerReadsThisServer) ProvidersOfHarness(harness msg.Harness) ([]string, error) {
	for _, described := range reads.server.discoverHarnesses() {
		if described.Name == string(harness) {
			return described.SupportedProviders, nil
		}
	}
	return nil, fmt.Errorf("this server does not know harness %q", harness)
}

func (reads schedulerReadsThisServer) CanonicalRoleNames() []string {
	return models.CanonicalRoles
}

func (reads schedulerReadsThisServer) RoleModelLists() (map[string][]string, error) {
	if reads.server.modelStore == nil {
		return nil, errModelStoreOff
	}
	return reads.server.modelStore.RoleModelLists()
}
