package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/rulestore"
	schedulerapi "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/api"
	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	schedulercron "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/cron"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db/dbtest"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enableSchedulerOnTestDatabase serves a scheduler with runs switched off,
// on a Postgres database of the test's own, as main does while
// scheduler.runs.enabled is off.
func enableSchedulerOnTestDatabase(t *testing.T, server *Server) {
	t.Helper()
	store := dbtest.NewStore(t)
	settings, err := schedulerconfig.NewStoredSettingsRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.AttachStoredValues(store.ServiceSettingValues(), nil); err != nil {
		t.Fatal(err)
	}
	scheduler := schedulercron.New(store)
	scheduler.SwitchRunsOff("runs are off in this test")
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	server.EnableScheduler(schedulerapi.New(store, scheduler, "", settings, func(string) error { return nil }).Routes())
}

// The scheduler answers under /scheduler/ with its own paths, to the operator
// only, and runs nothing while its runs are off.
func TestSchedulerRoutesAnswerUnderThePrefixToTheOperatorOnly(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	if off := gated.requestAsService(t, "GET", "/scheduler/api/jobs"); off.Code != http.StatusServiceUnavailable || !strings.Contains(off.Body.String(), "scheduler_off") {
		t.Fatalf("before the module opens = %d %s, want 503 scheduler_off", off.Code, off.Body)
	}
	enableSchedulerOnTestDatabase(t, gated.server)

	created := gated.requestAsServiceWithJSON(t, "POST", "/scheduler/api/jobs", map[string]any{"name": "disk check", "schedule": "*/5 * * * *", "command": "echo disk"})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"id":1`) {
		t.Fatalf("POST a job = %d %s", created.Code, created.Body)
	}
	reference := gated.requestAsService(t, "GET", "/scheduler/api/job-references/scheduler_job_000001")
	if reference.Code != http.StatusOK || !strings.Contains(reference.Body.String(), `"reference_id":"scheduler_job_000001"`) {
		t.Errorf("GET the job by its reference id = %d %s", reference.Code, reference.Body)
	}
	if run := gated.requestAsService(t, "POST", "/scheduler/api/jobs/1/run"); run.Code != http.StatusServiceUnavailable || !strings.Contains(run.Body.String(), "runs no scheduler jobs") {
		t.Errorf("run now while runs are off = %d %s, want 503", run.Code, run.Body)
	}
	if runs := gated.requestAsService(t, "GET", "/scheduler/api/jobs/1/runs"); runs.Code != http.StatusOK || strings.TrimSpace(runs.Body.String()) != "[]" {
		t.Errorf("the job's runs = %d %s, want none", runs.Code, runs.Body)
	}
	settings := gated.requestAsService(t, "GET", "/scheduler/settings")
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), schedulerconfig.SettingAgentJobModelRole) || strings.Contains(settings.Body.String(), "listen_host") {
		t.Errorf("GET /scheduler/settings = %d %s, want the stored model roles and nothing of the standalone daemon", settings.Code, settings.Body)
	}
	if refused := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/scheduler/api/jobs", nil); refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "operator_route") {
		t.Errorf("a principal who is not an administrator = %d %s, want 403 operator_route", refused.Code, refused.Body)
	}
	if administrator := gated.requestAsAdministrator(t, "GET", "/scheduler/api/jobs/1", nil); administrator.Code != http.StatusOK {
		t.Errorf("an administrator = %d %s, want 200", administrator.Code, administrator.Body)
	}
}

// This server's own callers of the scheduler reach it in process, with no
// address configured.
func TestTheSchedulerClientReachesTheSchedulerInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableSchedulerOnTestDatabase(t, gated.server)
	if created := gated.requestAsServiceWithJSON(t, "POST", "/scheduler/api/jobs", map[string]any{"name": "nightly digest", "schedule": "0 6 * * *", "command": "true"}); created.Code != http.StatusCreated {
		t.Fatalf("POST a job = %d %s", created.Code, created.Body)
	}
	job, refusal := gated.server.schedulerJob(context.Background(), 1)
	if refusal != nil || job.Name != "nightly digest" {
		t.Fatalf("schedulerJob = %+v, %+v", job, refusal)
	}
	if _, refusal := gated.server.schedulerJob(context.Background(), 2); refusal == nil || refusal.code != "unknown_record" {
		t.Errorf("a job the scheduler does not have = %+v, want unknown_record", refusal)
	}
	if outcome := gated.server.runSchedulerJobAction(1); outcome.err == nil || !strings.Contains(outcome.output, "runs no scheduler jobs") {
		t.Errorf("a scheduler job action while runs are off = %+v, want the scheduler's 503", outcome)
	}
}

// rule-store answers under /rule-store/ to the operator only, and with its
// reactor switched off an activation decides nothing.
func TestRuleStoreRoutesAnswerAndDecideNothingWhileTheReactorIsOff(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	if off := gated.requestAsService(t, "GET", "/rule-store/rules"); off.Code != http.StatusServiceUnavailable || !strings.Contains(off.Body.String(), "rule_store_off") {
		t.Fatalf("before the module opens = %d %s, want 503 rule_store_off", off.Code, off.Body)
	}
	module, err := rulestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	store := rulestore.New(postgrestest.NewDatabase(t, module).Open(module))
	subjects, err := store.ObserveSubjects([]rulestore.SubjectObservation{{Type: rulestore.SubjectTypeSession, ID: "br_1", State: "completed", Snapshot: json.RawMessage(`{"session_id":"br_1","state":"completed"}`)}})
	if err != nil || len(subjects) != 1 {
		t.Fatalf("ObserveSubjects = %v, %v", subjects, err)
	}
	bridge := &rulestore.LLMBridge{}
	kanban := &rulestore.KanbanStore{}
	reactor := rulestore.NewReactor(store, bridge, kanban, rulestore.NewOwnerCalls(bridge, kanban), 0)
	reactor.SwitchOff("the reactor is off in this test")
	routes := http.NewServeMux()
	rulestore.RegisterHandlers(context.Background(), routes, store, reactor, kanban)
	gated.server.EnableRuleStore(routes)

	created := gated.requestAsServiceWithJSON(t, "POST", "/rule-store/rules", map[string]any{"name": "completed sessions", "source": rulestore.SourceSession, "created_by": "principal_000001"})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"id":"rule_000001"`) {
		t.Fatalf("POST a rule = %d %s", created.Code, created.Body)
	}
	definition := map[string]any{"conditions": []any{map[string]any{"kind": "field_in", "field": "state", "values": []string{"completed"}}},
		"effects": []any{map[string]any{"kind": "add_session_tag", "tag": "done"}}}
	if revision := gated.requestAsServiceWithJSON(t, "POST", "/rule-store/rules/rule_000001/revisions", map[string]any{"definition": definition, "created_by": "principal_000001"}); revision.Code != http.StatusCreated {
		t.Fatalf("POST a revision = %d %s", revision.Code, revision.Body)
	}
	if activated := gated.requestAsServiceWithJSON(t, "POST", "/rule-store/rules/rule_000001/activate", map[string]any{"revision": 1, "mode": "shadow", "acted_by": "principal_000001", "reason": "test"}); activated.Code != http.StatusOK {
		t.Fatalf("activate = %d %s", activated.Code, activated.Body)
	}
	health := gated.requestAsService(t, "GET", "/rule-store/health")
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"switched_off_because":"the reactor is off in this test"`) || !strings.Contains(health.Body.String(), `"runs":0`) {
		t.Errorf("GET /rule-store/health = %d %s, want the reactor switched off and no run", health.Code, health.Body)
	}
	if refused := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/rule-store/rules", nil); refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "operator_route") {
		t.Errorf("a principal who is not an administrator = %d %s, want 403 operator_route", refused.Code, refused.Body)
	}
}

// rule-store reads this server's sessions and follows its session feed in
// process, through HTTPClientToThisServer, as the internal service.
func TestRuleStoreReadsSessionsAndTheirFeedFromThisServerInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	baseURL, client := gated.server.HTTPClientToThisServer(0)
	bridge := &rulestore.LLMBridge{BaseURL: baseURL, Client: client, Stream: client}
	if _, err := bridge.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hello := make(chan rulestore.FeedHello, 1)
	followed := make(chan error, 1)
	go func() {
		_, err := bridge.FollowSessions(ctx, "", rulestore.FeedHandler{
			Hello:   func(frame rulestore.FeedHello) error { hello <- frame; return nil },
			Upsert:  func(json.RawMessage) error { return nil },
			Delete:  func(string) error { return nil },
			Unknown: func(string) error { return nil },
		})
		followed <- err
	}()
	select {
	case <-hello:
	case err := <-followed:
		t.Fatalf("the feed ended before its hello: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no hello on the session feed within 10 seconds")
	}
}
