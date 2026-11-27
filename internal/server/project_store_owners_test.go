package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
	"github.com/kayushkin/llm-bridge/msg"
)

// project-store's owners in this process answer what their routes answered:
// the record for an id they have, projects.ErrNotFound for one they do
// not, and the route's refusal for a request it would refuse.
func TestProjectStoreOwnersAnswerInProcess(t *testing.T) {
	ctx := context.Background()
	test := newKanbanTestServer(t)
	srv := test.server
	repos := enableRepositoriesAndReferencesOnTestDatabase(t, srv)
	repo := &repostore.Repo{Name: "webapp", GitRemote: "git@github.com:example-owner/webapp.git"}
	if _, err := repos.UpsertRepo(repo); err != nil {
		t.Fatal(err)
	}

	repositories := srv.ProjectStoreRepositories()
	if name, err := repositories.RepoName(ctx, repo.ID); err != nil || name != "webapp" {
		t.Errorf("RepoName = %q, %v", name, err)
	}
	if _, err := repositories.RepoName(ctx, repo.ID+100); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("an unknown repo: %v, want ErrNotFound", err)
	}
	if deploys, err := repositories.Deployments(ctx, repo.ID, 1); err != nil || len(deploys) != 0 {
		t.Errorf("Deployments = %+v, %v", deploys, err)
	}
	if pullRequests, err := repositories.PullRequests(ctx, repo.ID, "all"); err != nil || len(pullRequests) != 0 {
		t.Errorf("PullRequests = %+v, %v", pullRequests, err)
	}
	if rows, err := repositories.SessionRepos(ctx, 0); err != nil || len(rows) != 0 {
		t.Errorf("SessionRepos = %+v, %v", rows, err)
	}

	kanban := srv.ProjectStoreKanban()
	if name, err := kanban.BoardName(ctx, test.seenBoardID); err != nil || name != "Support" {
		t.Errorf("BoardName = %q, %v", name, err)
	}
	if _, err := kanban.BoardName(ctx, "board-nobody-has"); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("an unknown board: %v, want ErrNotFound", err)
	}
	if cards, err := kanban.ProjectCards(ctx, "project_000001"); err != nil || len(cards) != 0 {
		t.Errorf("ProjectCards of a project no card links = %+v, %v", cards, err)
	}

	newSessionForSignals(t, srv.store, "br_chat", msg.SessionTypeInteractive)
	bridge := srv.ProjectStoreBridge()
	if session, err := bridge.Session(ctx, "br_chat"); err != nil || session.SessionID != "br_chat" || session.Type != "interactive" {
		t.Errorf("Session = %+v, %v", session, err)
	}
	if _, err := bridge.Session(ctx, "br_nobody"); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("an unknown session: %v, want ErrNotFound", err)
	}
	if brief, err := bridge.SessionBrief(ctx, "br_chat"); err != nil || brief.Brief != nil {
		t.Errorf("the brief of a chat no turn has briefed = %+v, %v", brief, err)
	}
	if _, err := bridge.SessionBrief(ctx, "br_nobody"); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("the brief of an unknown session: %v, want ErrNotFound", err)
	}
	if _, err := bridge.OneShot(ctx, msg.OneShotRequest{ModelRole: "balanced"}); err == nil || !strings.Contains(err.Error(), "prompt_required") {
		t.Errorf("a one-shot with no prompt: %v, want prompt_required as POST /oneshot refuses it", err)
	}
	if _, err := bridge.OneShot(ctx, msg.OneShotRequest{Prompt: "hi", Model: "some-model"}); err == nil || !strings.Contains(err.Error(), "model_not_accepted") {
		t.Errorf("a one-shot naming a model: %v, want model_not_accepted", err)
	}

	if _, err := srv.ProjectStoreScheduler().JobName(ctx, 76); err == nil {
		t.Error("a scheduler that is not open answered a job name")
	}
	srv.enableSchedulerParts(nil, newFakeSessionActionStores())
	if name, err := srv.ProjectStoreScheduler().JobName(ctx, 76); err != nil || name != "nightly-digest" {
		t.Errorf("JobName = %q, %v", name, err)
	}
	if _, err := srv.ProjectStoreScheduler().JobName(ctx, 5); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("an unknown job: %v, want ErrNotFound", err)
	}
}

// The chats the session filer reads are the interactive chat sessions, a
// page at a time.
func TestProjectStoreReadsChatSessionsInProcess(t *testing.T) {
	srv, st := testServer(t)
	for _, id := range []string{"br_1", "br_2", "br_3"} {
		if err := st.CreateSession(&store.Session{SessionID: id, Harness: "claude_code", State: "idle", Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat}); err != nil {
			t.Fatal(err)
		}
	}
	newSessionForSignals(t, st, "br_worker", msg.SessionTypeAutonomous)
	bridge := srv.ProjectStoreBridge()
	first, next, err := bridge.ChatSessions(context.Background(), "", 2)
	if err != nil || len(first) != 2 || next == "" {
		t.Fatalf("first page = %+v, %q, %v", first, next, err)
	}
	rest, last, err := bridge.ChatSessions(context.Background(), next, 2)
	if err != nil || len(rest) != 1 || last != "" {
		t.Fatalf("second page = %+v, %q, %v", rest, last, err)
	}
	seen := map[string]bool{}
	for _, chat := range append(first, rest...) {
		seen[chat.SessionID] = true
	}
	if len(seen) != 3 || seen["br_worker"] {
		t.Errorf("chats = %v, want the three chats and not the worker", seen)
	}
}
