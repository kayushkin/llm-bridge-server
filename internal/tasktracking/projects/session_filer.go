package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The session filer files chat sessions under projects. A pass lists the
// chats llm-bridge-server changed in a window, leaves alone the ones a person
// or another agent filed and the ones it already judged whose goal has not
// changed since, and for each of the rest asks a model which project the chat
// belongs to, from the chat's brief (its goal and current step) and the repos
// it moved refs in. A pick the model is sure enough of becomes a link; anything
// else becomes a proposal a person decides. The filer never makes a project.
//
// Nothing tells this service that a brief was written, so a scheduler job
// runs a pass every few minutes; a backfill is the same pass over a longer
// window.

// SessionFilerName is created_by on every link and filing the filer writes.
const SessionFilerName = "project-store session filer"

// SessionFilerCaller is how llm-bridge-server's logs name the filer's model calls.
const SessionFilerCaller = "project-store-session-filer"

// sessionFilerConcurrentModelCalls is how many sessions one pass judges at once.
const sessionFilerConcurrentModelCalls = 4

// sessionFilerSummaryPageSize is how many chats one page of the bridge's list holds.
const sessionFilerSummaryPageSize = 500

// MaximumSessionFilerWindowDays is the longest window a pass may be asked for.
const MaximumSessionFilerWindowDays = 365

// What llm-bridge-server calls a chat a person has with a harness, and the
// origin of a session it did not start itself.
const (
	bridgeSessionTypeInteractive = "interactive"
	bridgeSessionPurposeChat     = "chat"
	bridgeSessionOriginDiscovery = "discovery"
)

// SessionFiler runs filing passes.
type SessionFiler struct {
	Store  *Store
	Owners *Owners
	// ModelRole is the model-store role asked, through llm-bridge-server's
	// one-shot router. Empty refuses every pass.
	ModelRole string
	// MinimumConfidence is the lowest odds at which the filer files a session
	// by itself; below it the pick is a proposal.
	MinimumConfidence float64
	// QuietBeforeFilingWithoutABrief is how long a chat with no brief must
	// have been unchanged before it is judged from its name alone. A chat
	// gets its brief when its first turn ends, so a shorter wait would judge
	// chats whose brief is about to be written.
	QuietBeforeFilingWithoutABrief time.Duration
	// Window is how far back a pass looks when the caller names no window.
	Window time.Duration
	// SessionsPerPass is how many sessions a pass judges when the caller
	// names no limit.
	SessionsPerPass int

	running sync.Mutex
}

// Validate refuses settings no pass could run with, so a bad value stops the
// service at start instead of filing nothing, or everything, later.
func (filer *SessionFiler) Validate() error {
	switch {
	case filer.MinimumConfidence <= 0 || filer.MinimumConfidence > 1:
		return fmt.Errorf("the session filer's minimum confidence is %v; it must be above 0 and at most 1", filer.MinimumConfidence)
	case filer.Window <= 0:
		return fmt.Errorf("the session filer's window is %s; it must be longer than nothing", filer.Window)
	case filer.SessionsPerPass < 1:
		return fmt.Errorf("the session filer judges %d sessions a pass; it must be at least 1", filer.SessionsPerPass)
	case filer.QuietBeforeFilingWithoutABrief < 0:
		return fmt.Errorf("the session filer's wait before filing without a brief is %s; it must not be negative", filer.QuietBeforeFilingWithoutABrief)
	}
	return nil
}

// SessionFilerRunRequest is the body of POST /session-filings/run.
type SessionFilerRunRequest struct {
	// SinceDays is how many days back the pass looks; 0 means the filer's window.
	SinceDays int
	// Limit is the most sessions the pass judges; 0 means the filer's own.
	Limit int
}

// chatSessionsChangedSince lists the chats llm-bridge-server changed since
// then, newest first. The bridge pages its list by the text of updated_at,
// which it has stored in more than one time zone, so the pages are only
// roughly in time order: reading goes on until a page ends a day before the
// window, and each row is kept or dropped by its own time.
func (owners *Owners) chatSessionsChangedSince(ctx context.Context, since time.Time) ([]BridgeChatSession, error) {
	bridge, err := owners.bridge("the chat sessions")
	if err != nil {
		return nil, fmt.Errorf("list chat sessions: %w", err)
	}
	var chats []BridgeChatSession
	seen := map[string]bool{}
	cursor := ""
	for {
		page, next, err := bridge.ChatSessions(ctx, cursor, sessionFilerSummaryPageSize)
		if err := askOwner("llm-bridge-server, the chat sessions", err); err != nil {
			return nil, fmt.Errorf("list chat sessions: %w", err)
		}
		for _, chat := range page {
			if chat.UpdatedAt.Before(since) || seen[chat.SessionID] {
				continue
			}
			seen[chat.SessionID] = true
			chats = append(chats, chat)
		}
		if next == "" || len(page) == 0 || next == cursor {
			break
		}
		if page[len(page)-1].UpdatedAt.Before(since.Add(-24 * time.Hour)) {
			break
		}
		cursor = next
	}
	sort.SliceStable(chats, func(i, j int) bool { return chats[i].UpdatedAt.After(chats[j].UpdatedAt) })
	return chats, nil
}

// sessionBriefsChangedSince reads every brief llm-bridge-server changed since
// then, by session id.
func (owners *Owners) sessionBriefsChangedSince(ctx context.Context, since time.Time) (map[string]msg.SessionBriefView, error) {
	bridge, err := owners.bridge("the session briefs")
	if err != nil {
		return nil, fmt.Errorf("read session briefs: %w", err)
	}
	views, err := bridge.SessionBriefsUpdatedSince(ctx, since)
	if err := askOwner("llm-bridge-server, the session briefs", err); err != nil {
		return nil, fmt.Errorf("read session briefs: %w", err)
	}
	briefs := make(map[string]msg.SessionBriefView, len(views))
	for _, view := range views {
		if view.Brief != nil {
			briefs[view.Brief.SessionID] = view
		}
	}
	return briefs, nil
}

// sessionReposSince reads, from work-graph-store, the repos each session moved
// refs in since then, by session id, the most moved first.
func (owners *Owners) sessionReposSince(ctx context.Context, since time.Time) (map[string][]SessionRepo, error) {
	repositories, err := owners.repositories("the repos sessions changed")
	if err != nil {
		return nil, fmt.Errorf("read the repos sessions changed: %w", err)
	}
	rows, err := repositories.SessionRepos(ctx, since.UnixNano())
	if err := askOwner("work-graph-store, the repos sessions changed", err); err != nil {
		return nil, fmt.Errorf("read the repos sessions changed: %w", err)
	}
	repos := map[string][]SessionRepo{}
	for _, row := range rows {
		repos[row.SessionID] = append(repos[row.SessionID], row)
	}
	for _, list := range repos {
		sort.SliceStable(list, func(i, j int) bool { return list[i].RefUpdates > list[j].RefUpdates })
	}
	return repos, nil
}

// sessionFilerSkipReason says why the filer does not file a session, or ""
// when it does. It files the chats a person has through a frontend, and
// nothing a scheduler, an orchestrator sweep, another session or a harness run
// outside the bridge started.
func sessionFilerSkipReason(session BridgeSession) string {
	switch {
	case session.Type != bridgeSessionTypeInteractive:
		return "not a chat: type " + session.Type
	case session.Purpose != bridgeSessionPurposeChat:
		return "not a chat: purpose " + session.Purpose
	case session.Origin == bridgeSessionOriginDiscovery:
		return "ran outside the bridge: a one-shot call or a test run found on disk"
	case session.ManagerSessionID != "":
		return "started by another session"
	}
	return ""
}

// judgedAgain says whether a judged session is to be judged again. Three
// things bring one back: its brief records a changed goal after the filing;
// the filer judged it from its name, nobody has decided on that since, and it
// now has a brief; or its proposal still waits and a project has been made
// since, which may be the one it belongs to.
func judgedAgain(filing SessionFiling, brief msg.SessionBriefView, hasBrief bool, newestProjectCreatedAt int64) bool {
	if filing.Status == SessionFilingProposed && filing.CreatedAt < newestProjectCreatedAt {
		return true
	}
	if !hasBrief {
		return false
	}
	for _, revision := range brief.Revisions {
		if revision.GoalChanged && revision.CreatedAt.Unix() > filing.CreatedAt {
			return true
		}
	}
	undecided := filing.Status == SessionFilingFiled || filing.Status == SessionFilingProposed || filing.Status == SessionFilingNothingToFile
	return undecided && filing.Evidence != SessionFilingEvidenceBrief
}

// sessionFilerProject is a project as the model is shown it.
type sessionFilerProject struct {
	Project   Project
	RepoNames []string
}

// sessionToFile is everything the model is shown about one chat.
type sessionToFile struct {
	Session BridgeSession
	// Goal and CurrentStep are the brief's; both empty when there is none.
	Goal        string
	CurrentStep string
	Evidence    string
	Repos       []SessionRepo
}

const sessionFilerSystemPrompt = `You file one chat between a person and an AI assistant under the project it belongs to. You are shown the person's projects, each with its id, what it is for and the repos it owns, and what is known about the chat: its goal, where it stands, its name, the folder it ran in and the repos it changed.

A project is a piece of work: one thing being built or kept running. A chat belongs to it when the chat does that work: it changes the project's code or data, plans or reviews it, fixes or operates what it built, or uses what it built for the purpose it was built for. Sharing a subject with a project is not belonging to it: a general question about government budgets does not belong to a project that tracks the person's own money, and a question about a film does not belong to a project that builds a film library.

A stream never finishes and takes whatever its description says it takes. When a stream's description covers the chat, the chat belongs to that stream as it would to a project: a stream for questions and errands that are not software takes a general question, a health question or a shopping comparison.

Pick the one project or stream the chat belongs to.
- Prefer the most specific: a sub-project over its parent when the chat is about that part, and a project over a stream when the chat does the project's work.
- The goal decides. The repos the chat changed support it: a repo only one project owns is strong evidence, a repo several own is weak, and a chat often changes a shared repo for the sake of another project.
- Never pick a project for being the nearest of a poor lot. When the chat is real work that no project or stream covers, or too little is known to tell, answer with an empty project_id: the person will decide, and may need a new project. Never answer with an id that is not in the list.

nothing_to_file is true only when the chat has no aim at all: a test message ("reply with ok"), a greeting with nothing after it, or a chat that never got to a request. Then project_id is empty. A chat with any real question or task is never nothing_to_file.

confidence is the odds, from 0 to 1, that the person would file this chat under that project if asked:
- 0.95: the goal names the project's own work outright, and the repos agree or there are none.
- 0.9: the goal is plainly this project's work though it does not name it, and nothing points elsewhere.
- 0.7: it probably belongs here, but another project, or none, is a real possibility.
- 0.5: two fit about equally and this is the likelier, or the fit rests on a shared subject more than on the project's work.
- 0.3 or less: a guess.
When the goal is not known, the name and the repos are all there is. A name that states the work plainly, backed by repos that one project owns, can still earn 0.9; a vague name with no repos cannot.
With an empty project_id, confidence is 0.

why is one plain sentence naming the evidence, or what is missing.`

func sessionFilerOutputSchema(projects []sessionFilerProject) json.RawMessage {
	ids := []string{""}
	for _, project := range projects {
		ids = append(ids, project.Project.ID)
	}
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id":      map[string]any{"type": "string", "enum": ids, "description": "The id of the project the chat belongs to, or empty when none fits."},
			"nothing_to_file": map[string]any{"type": "boolean", "description": "True only for a chat with no aim at all: a test message, a bare greeting."},
			"confidence":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"why":             map[string]any{"type": "string", "description": "One sentence naming the evidence."},
		},
		"required": []string{"project_id", "nothing_to_file", "confidence", "why"},
	})
	return schema
}

// sessionFilerPrompt lays out the projects and the chat for the model.
func sessionFilerPrompt(projects []sessionFilerProject, session sessionToFile, repoNames map[int64]string, projectsByRepo map[int64][]Project) string {
	var prompt strings.Builder
	names := map[string]string{}
	for _, project := range projects {
		names[project.Project.ID] = project.Project.Name
	}
	prompt.WriteString("## Projects\n\n")
	for _, entry := range projects {
		project := entry.Project
		fmt.Fprintf(&prompt, "- %s %q (%s, stage %s", project.ID, project.Name, project.Kind, project.Stage)
		if project.ParentID != "" {
			fmt.Fprintf(&prompt, ", part of %s %q", project.ParentID, names[project.ParentID])
		}
		prompt.WriteString(")\n")
		if project.Goal != "" {
			fmt.Fprintf(&prompt, "  For: %s\n", project.Goal)
		}
		if len(entry.RepoNames) > 0 {
			fmt.Fprintf(&prompt, "  Repos: %s\n", strings.Join(entry.RepoNames, ", "))
		}
	}
	prompt.WriteString("\n## The chat\n\n")
	fmt.Fprintf(&prompt, "Name: %s\n", session.Session.DisplayName)
	if session.Evidence == SessionFilingEvidenceBrief {
		fmt.Fprintf(&prompt, "Goal: %s\n", session.Goal)
		if session.CurrentStep != "" {
			fmt.Fprintf(&prompt, "Where it stands: %s\n", session.CurrentStep)
		}
	} else {
		prompt.WriteString("Goal: not known. No brief was written for this chat, so its name is all that says what it was about.\n")
	}
	if session.Session.WorkingDirectory != "" {
		fmt.Fprintf(&prompt, "Folder it ran in: %s\n", session.Session.WorkingDirectory)
	}
	if len(session.Repos) == 0 {
		prompt.WriteString("Repos it changed: none recorded.\n")
	} else {
		prompt.WriteString("Repos it changed, the most changed first:\n")
		for _, repo := range session.Repos {
			var owners []string
			for _, project := range projectsByRepo[repo.RepoStoreID] {
				owners = append(owners, fmt.Sprintf("%s %q", project.ID, project.Name))
			}
			owned := "no project owns it"
			if len(owners) > 0 {
				owned = "owned by " + strings.Join(owners, ", ")
			}
			fmt.Fprintf(&prompt, "- %s: %d ref moves; %s\n", repoNames[repo.RepoStoreID], repo.RefUpdates, owned)
		}
	}
	return prompt.String()
}

// sessionFilerVerdict is the model's answer.
type sessionFilerVerdict struct {
	ProjectID     string  `json:"project_id"`
	NothingToFile bool    `json:"nothing_to_file"`
	Confidence    float64 `json:"confidence"`
	Why           string  `json:"why"`
}

// askWhichProject makes the one model call for a session.
func (filer *SessionFiler) askWhichProject(ctx context.Context, prompt string, schema json.RawMessage) (sessionFilerVerdict, string, error) {
	bridge, err := filer.Owners.bridge("a model call")
	if err != nil {
		return sessionFilerVerdict{}, "", fmt.Errorf("ask a model which project the chat belongs to: %w", err)
	}
	modelContext, cancel := context.WithTimeout(ctx, modelCallTimeout)
	defer cancel()
	answer, err := bridge.OneShot(modelContext, msg.OneShotRequest{Prompt: prompt, SystemPrompt: sessionFilerSystemPrompt, ModelRole: filer.ModelRole,
		Caller: SessionFilerCaller, Schema: schema})
	if err := askOwner("llm-bridge-server, the one-shot router", err); err != nil {
		return sessionFilerVerdict{}, "", fmt.Errorf("ask a model which project the chat belongs to: %w", err)
	}
	if len(answer.Parsed) == 0 {
		return sessionFilerVerdict{}, answer.Model, fmt.Errorf("%w: the model (%s) answered with no structured output", ErrOwnerUnavailable, answer.Model)
	}
	var verdict sessionFilerVerdict
	if err := json.Unmarshal(answer.Parsed, &verdict); err != nil {
		return sessionFilerVerdict{}, answer.Model, fmt.Errorf("%w: the model's (%s) answer does not fit the schema: %v", ErrOwnerUnavailable, answer.Model, err)
	}
	return verdict, answer.Model, nil
}

// filingFromVerdict turns the model's answer into the filing to record. A
// project the model was not shown, or odds outside 0 to 1, is an error: the
// filer never files under, or proposes, a project that does not exist.
func (filer *SessionFiler) filingFromVerdict(session sessionToFile, verdict sessionFilerVerdict, model string, projects []sessionFilerProject) (SessionFiling, error) {
	filing := SessionFiling{SessionID: session.Session.SessionID, SessionLabel: session.Session.DisplayName, Evidence: session.Evidence,
		Goal: session.Goal, ProjectID: strings.TrimSpace(verdict.ProjectID), Confidence: verdict.Confidence, Why: strings.TrimSpace(verdict.Why),
		Model: model, CreatedBy: SessionFilerName, RepoStoreIDs: []int64{}}
	for _, repo := range session.Repos {
		filing.RepoStoreIDs = append(filing.RepoStoreIDs, repo.RepoStoreID)
	}
	if filing.Confidence < 0 || filing.Confidence > 1 {
		return SessionFiling{}, fmt.Errorf("%w: the model (%s) gave odds of %v, which is not between 0 and 1", ErrOwnerUnavailable, model, verdict.Confidence)
	}
	if verdict.NothingToFile {
		filing.ProjectID, filing.Confidence, filing.Status = "", 0, SessionFilingNothingToFile
		return filing, nil
	}
	if filing.ProjectID == "" {
		filing.Confidence = 0
		filing.Status = SessionFilingProposed
		return filing, nil
	}
	shown := false
	for _, project := range projects {
		shown = shown || project.Project.ID == filing.ProjectID
	}
	if !shown {
		return SessionFiling{}, fmt.Errorf("%w: the model (%s) picked %q, which is not a project it was shown", ErrOwnerUnavailable, model, filing.ProjectID)
	}
	filing.Status = SessionFilingProposed
	if filing.Confidence >= filer.MinimumConfidence {
		filing.Status = SessionFilingFiled
	}
	return filing, nil
}

// projectsForTheModel lists the live projects, each parent before its
// children, with the names repo-store has for the repos each owns, and which
// projects own each repo.
func (filer *SessionFiler) projectsForTheModel(ctx context.Context, repoNames map[int64]string) ([]sessionFilerProject, map[int64][]Project, error) {
	projects, err := filer.Store.Projects(ProjectFilter{})
	if err != nil {
		return nil, nil, err
	}
	repoLinks, err := filer.Store.Links(LinkFilter{EntityType: EntityRepo})
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]Project{}
	children := map[string][]Project{}
	for _, project := range projects {
		byID[project.ID] = project
	}
	for _, project := range projects {
		parent := project.ParentID
		if _, live := byID[parent]; !live {
			parent = ""
		}
		children[parent] = append(children[parent], project)
	}
	reposOf := map[string][]string{}
	projectsByRepo := map[int64][]Project{}
	for _, link := range repoLinks {
		project, live := byID[link.ProjectID]
		if !live {
			continue
		}
		repoStoreID, err := strconv.ParseInt(link.EntityRef, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("repo link %d of %s: %q is not a repo-store id", link.ID, link.ProjectID, link.EntityRef)
		}
		name, err := filer.repoName(ctx, repoStoreID, repoNames)
		if err != nil {
			return nil, nil, err
		}
		reposOf[project.ID] = append(reposOf[project.ID], name)
		projectsByRepo[repoStoreID] = append(projectsByRepo[repoStoreID], project)
	}
	var ordered []sessionFilerProject
	var walk func(parentID string)
	walk = func(parentID string) {
		for _, project := range children[parentID] {
			ordered = append(ordered, sessionFilerProject{Project: project, RepoNames: reposOf[project.ID]})
			walk(project.ID)
		}
	}
	walk("")
	return ordered, projectsByRepo, nil
}

// repoName reads a repo's name from repo-store once a pass. A repo repo-store
// no longer has is named by its id, so the model still sees that it was changed.
func (filer *SessionFiler) repoName(ctx context.Context, repoStoreID int64, repoNames map[int64]string) (string, error) {
	if name, known := repoNames[repoStoreID]; known {
		return name, nil
	}
	name, err := filer.Owners.RepoName(ctx, repoStoreID)
	if errors.Is(err, ErrNotFound) {
		name, err = fmt.Sprintf("a repo repo-store no longer has (id %d)", repoStoreID), nil
	}
	if err != nil {
		return "", err
	}
	repoNames[repoStoreID] = name
	return name, nil
}

// Run does one filing pass. One pass runs at a time; a second is a conflict.
func (filer *SessionFiler) Run(ctx context.Context, request SessionFilerRunRequest) (SessionFilerReport, error) {
	if filer.ModelRole == "" {
		return SessionFilerReport{}, fmt.Errorf("%w: no model role is set for the session filer (projects.session_filer.model_role)", ErrInvalid)
	}
	if request.SinceDays < 0 || request.SinceDays > MaximumSessionFilerWindowDays {
		return SessionFilerReport{}, fmt.Errorf("%w: since_days must be from 1 to %d", ErrInvalid, MaximumSessionFilerWindowDays)
	}
	if request.Limit < 0 {
		return SessionFilerReport{}, fmt.Errorf("%w: limit must be a positive whole number", ErrInvalid)
	}
	if !filer.running.TryLock() {
		return SessionFilerReport{}, fmt.Errorf("%w: a filing pass is already running", ErrConflict)
	}
	defer filer.running.Unlock()

	now := filer.Owners.now()
	since := now.Add(-filer.Window)
	if request.SinceDays > 0 {
		since = now.AddDate(0, 0, -request.SinceDays)
	}
	limit := filer.SessionsPerPass
	if request.Limit > 0 {
		limit = request.Limit
	}
	report := SessionFilerReport{Since: since.Unix(), AlreadyJudged: map[string]int{}, Skipped: map[string]int{}, Judged: []SessionFiling{}, Failed: []SessionFilerFailure{}}

	chats, err := filer.Owners.chatSessionsChangedSince(ctx, since)
	if err != nil {
		return report, err
	}
	report.ChatSessions = len(chats)
	sessionLinks, err := filer.Store.Links(LinkFilter{EntityType: EntitySession})
	if err != nil {
		return report, err
	}
	filedByOthers := map[string]bool{}
	for _, link := range sessionLinks {
		if link.CreatedBy != SessionFilerName {
			filedByOthers[link.EntityRef] = true
		}
	}
	latest, err := filer.Store.LatestSessionFilings()
	if err != nil {
		return report, err
	}
	briefs, err := filer.Owners.sessionBriefsChangedSince(ctx, since)
	if err != nil {
		return report, err
	}
	repoNames := map[int64]string{}

	projects, projectsByRepo, err := filer.projectsForTheModel(ctx, repoNames)
	if err != nil {
		return report, err
	}
	var newestProjectCreatedAt int64
	for _, project := range projects {
		newestProjectCreatedAt = max(newestProjectCreatedAt, project.Project.CreatedAt)
	}

	// The sessions to look at: the chats changed in the window, then, from
	// any time, the ones whose proposal still waits and may fit a project
	// made since.
	sessionIDs := make([]string, 0, len(chats))
	inWindow := map[string]bool{}
	for _, chat := range chats {
		sessionIDs = append(sessionIDs, chat.SessionID)
		inWindow[chat.SessionID] = true
	}
	var waitingOnANewProject []string
	for sessionID, filing := range latest {
		if !inWindow[sessionID] && filing.Status == SessionFilingProposed && filing.CreatedAt < newestProjectCreatedAt {
			waitingOnANewProject = append(waitingOnANewProject, sessionID)
		}
	}
	sort.Strings(waitingOnANewProject)
	sessionIDs = append(sessionIDs, waitingOnANewProject...)

	bridge, err := filer.Owners.bridge("the sessions to file")
	if err != nil {
		return report, err
	}
	// Which of them need judging: unfiled by anyone else, and either never
	// judged or due to be judged again.
	var toJudge []sessionToFile
	earliestCreated := now
	for _, sessionID := range sessionIDs {
		if filedByOthers[sessionID] {
			if inWindow[sessionID] {
				report.FiledByOthers++
			}
			continue
		}
		brief, hasBrief := briefs[sessionID]
		filing, judged := latest[sessionID]
		if judged && !judgedAgain(filing, brief, hasBrief, newestProjectCreatedAt) {
			report.AlreadyJudged[filing.Status]++
			continue
		}
		record, err := bridge.Session(ctx, sessionID)
		if err := askOwner("llm-bridge-server, session "+sessionID, err); err != nil {
			report.Failed = append(report.Failed, SessionFilerFailure{SessionID: sessionID, Error: fmt.Sprintf("read the session: %v", err)})
			continue
		}
		// An old row keeps its id in another column; the id this pass files
		// by is the one the bridge listed it under.
		record.SessionID = sessionID
		if reason := sessionFilerSkipReason(record); reason != "" {
			report.Skipped[reason]++
			continue
		}
		if !hasBrief {
			// The brief may be older than the window while the session
			// changed inside it.
			brief, err = bridge.SessionBrief(ctx, sessionID)
			if err := askOwner("llm-bridge-server, the brief of session "+sessionID, err); err != nil {
				report.Failed = append(report.Failed, SessionFilerFailure{SessionID: sessionID, Error: fmt.Sprintf("read the brief: %v", err)})
				continue
			}
			hasBrief = brief.Brief != nil
			if judged && !judgedAgain(filing, brief, hasBrief, newestProjectCreatedAt) {
				report.AlreadyJudged[filing.Status]++
				continue
			}
		}
		session := sessionToFile{Session: record, Evidence: SessionFilingEvidenceName}
		switch {
		case hasBrief:
			session.Evidence, session.Goal, session.CurrentStep = SessionFilingEvidenceBrief, brief.Brief.OverallGoal, brief.Brief.CurrentStep
		case now.Sub(record.UpdatedAt) < filer.QuietBeforeFilingWithoutABrief:
			report.WaitingForABrief++
			continue
		case strings.TrimSpace(record.DisplayName) == "":
			session.Evidence = SessionFilingEvidenceNone
		}
		if record.CreatedAt.Before(earliestCreated) {
			earliestCreated = record.CreatedAt
		}
		toJudge = append(toJudge, session)
	}
	if len(toJudge) > limit {
		report.Remaining = len(toJudge) - limit
		toJudge = toJudge[:limit]
	}
	if len(toJudge) == 0 {
		return report, nil
	}

	// A session cannot have moved a ref before it was created.
	sessionRepos, err := filer.Owners.sessionReposSince(ctx, earliestCreated)
	if err != nil {
		return report, err
	}
	for index := range toJudge {
		toJudge[index].Repos = sessionRepos[toJudge[index].Session.SessionID]
		for _, repo := range toJudge[index].Repos {
			if _, err := filer.repoName(ctx, repo.RepoStoreID, repoNames); err != nil {
				return report, err
			}
		}
	}
	schema := sessionFilerOutputSchema(projects)

	type outcome struct {
		filing SessionFiling
		err    error
	}
	outcomes := make([]outcome, len(toJudge))
	var workers sync.WaitGroup
	slots := make(chan struct{}, sessionFilerConcurrentModelCalls)
	for index := range toJudge {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			outcomes[index].filing, outcomes[index].err = filer.judge(ctx, toJudge[index], projects, repoNames, projectsByRepo, schema)
		}(index)
	}
	workers.Wait()
	for index, result := range outcomes {
		if result.err != nil {
			report.Failed = append(report.Failed, SessionFilerFailure{SessionID: toJudge[index].Session.SessionID, Error: result.err.Error()})
			continue
		}
		report.Judged = append(report.Judged, result.filing)
	}
	return report, nil
}

// judge decides one session and records the filing.
func (filer *SessionFiler) judge(ctx context.Context, session sessionToFile, projects []sessionFilerProject, repoNames map[int64]string,
	projectsByRepo map[int64][]Project, schema json.RawMessage) (SessionFiling, error) {
	if session.Evidence == SessionFilingEvidenceNone {
		return filer.Store.RecordSessionFiling(SessionFiling{SessionID: session.Session.SessionID, Evidence: SessionFilingEvidenceNone,
			Status: SessionFilingNothingToFile, Why: "The chat has no name and no brief: nothing was said in it.", CreatedBy: SessionFilerName})
	}
	if err := ctx.Err(); err != nil {
		return SessionFiling{}, err
	}
	verdict, model, err := filer.askWhichProject(ctx, sessionFilerPrompt(projects, session, repoNames, projectsByRepo), schema)
	if err != nil {
		return SessionFiling{}, err
	}
	filing, err := filer.filingFromVerdict(session, verdict, model, projects)
	if err != nil {
		return SessionFiling{}, err
	}
	return filer.Store.RecordSessionFiling(filing)
}

func (handlers *Handlers) RunSessionFiler(writer http.ResponseWriter, request *http.Request) {
	fields, err := decodeFields(request, "since_days", "limit")
	if err != nil {
		writeError(writer, err)
		return
	}
	var run SessionFilerRunRequest
	for name, target := range map[string]*int{"since_days": &run.SinceDays, "limit": &run.Limit} {
		raw, present := fields[name]
		if !present {
			continue
		}
		if err := json.Unmarshal(raw, target); err != nil || *target < 1 {
			writeError(writer, fmt.Errorf("%w: %s must be a positive whole number", ErrInvalid, name))
			return
		}
	}
	if handlers.SessionFiler == nil {
		writeError(writer, fmt.Errorf("%w: this service runs no session filer", ErrInvalid))
		return
	}
	report, err := handlers.SessionFiler.Run(request.Context(), run)
	if err != nil {
		writeError(writer, err)
		return
	}
	// A pass that could not judge some session answers 502 with the whole
	// report, so the job that runs it shows the failure; what it did file
	// stays filed, and the next pass tries the rest again.
	status := http.StatusOK
	if len(report.Failed) > 0 {
		status = http.StatusBadGateway
	}
	writeJSON(writer, status, report)
}
