package sessionactions

import (
	"context"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repos"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sendanswer"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Dependencies is what session actions read from the server that composes
// them.
type Dependencies struct {
	Harness      *harness.Manager
	HarnessStore func() *machines.Store
	ModelStore   func() *models.Store
	// Mux answers a request this package makes of the server itself.
	Mux http.Handler
	// SchedulerMountPrefix is where the scheduler's routes are mounted, for
	// the link a run_scheduler_job report carries.
	SchedulerMountPrefix   string
	RepoRegistryForDeploys func() RepoRegistryForDeploys
	SessionActionModels    func() SessionActionModels
	Settings               *servicesettings.Registry
	Store                  *store.Store
	Sessions               Host
	ServerReads            ServerReads
	PostingEnvironments    PostingEnvironments
}

// Host is the sessions, callers and scheduler jobs a session action reaches.
type Host interface {
	SendMessageFromThisServer(ctx context.Context, sessionID string, req msg.SendMessageRequest, senderPrincipalID string) sendanswer.Answer
	StopSession(bridgeID string) error
}

// RepoRegistryForDeploys is what this package calls on the server's repoRegistryForDeploys.
type RepoRegistryForDeploys interface {
	GetRepo(id int64) (*repos.Repo, error)
}

// Service offers, lists and runs session actions (the chat's buttons).
type Service struct {
	Dependencies
}

// PostingEnvironments is the environment a session's current spawn gives its agent to post into the session.
type PostingEnvironments interface {
	SessionPostingEnvironmentOfCurrentSpawn(session *store.Session) []string
}

// ServerReads is who a principal is and the scheduler's jobs, as this server reads them.
type ServerReads interface {
	CallerForPrincipalID(ctx context.Context, principalID string, subjectDescription string) (requestcaller.Caller, *requestcaller.CredentialError)
	SchedulerJobsInProcess() SchedulerJobs
}
