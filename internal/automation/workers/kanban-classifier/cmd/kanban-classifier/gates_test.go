package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// Ported from 0530193e and 59414aad (test/first-tests-for-kanban-classifier),
// which pinned the gate stack against the direct-API classifier. Main has since
// moved the model call to llm-bridge-server's oneshot route and keyed the check
// signature on the finished response rather than on message count, state and
// minute, so the gates are the same and every fixture is new.
//
// The classifier's skip gates — final-classified, turn in flight, no finished
// response, do-not-track, and the check signature — are the only things standing
// between a 15-minute cron tick and a model call per session per tick. Nothing
// downstream re-checks them, and a gate that stops firing does not fail: it
// spends, and the run still reads normally.
//
// So the assertion that matters in this file is not what was written to the
// board. It is `world.modelCalls` — whether the model was reached at all.
// bounded_fetch_test.go pins the signature helpers one function at a time; this
// file pins that processSession consults them.

// classifierWorld stands in for kanban-store and llm-bridge-server. It records
// what the classifier did rather than what it was given, so a test reads the
// decision off the wire.
type classifierWorld struct {
	// Inputs.
	tags               []string
	cards              []map[string]any
	finishedResponseID string // "" gives a page whose only turn has no result entry
	eventCount         int
	replyStatus        int
	replyBody          string

	// Recorded.
	modelCalls    int
	sentPrompt    string
	tagsAdded     []string
	tagsDeleted   []string
	createdTitles []string
	patchedBodies map[string]string
	movedTo       map[string]string // card id -> column id

	cfg      config
	kc       *issuesclient.Client
	degraded *degradations.Recorder
}

// completeReply is a well-formed, untruncated classification. Tests about a gate
// rather than about the report use it, so the only reason nothing happens is the
// gate.
func completeReply(trackable bool, newTasks ...NewRec) string {
	report := map[string]any{"trackable": trackable, "session_summary": "s", "new_tasks": newTasks}
	raw, _ := json.Marshal(map[string]any{"stop_reason": "end_turn", "parsed": report})
	return string(raw)
}

// truncatedReply stopped at the output cap part-way through a new task.
func truncatedReply() string {
	return `{"stop_reason":"max_tokens","usage":{"output_tokens":8192},"parsed":{"trackable":true,` +
		`"new_tasks":[{"title":"Half a thought","state":"in_progress","summary":"s"}]}}`
}

// The response id and event count the stub page carries by default. The
// signature a classification of that page records is spelled out rather than
// built with buildSig: a test that asks the code under test what it expects
// cannot see that answer drift.
const (
	gatedResponseID   = "entry-result-gate"
	gatedEventCount   = 5000
	gatedSignature    = "kanban-check-sig:result=entry-result-gate:events=5000"
	supersededSigTag  = "kanban-check-sig:result=entry-result-older:events=4000"
	preResponseSigTag = "kanban-check-sig:msg=37:state=idle:t=202609162123"
)

func newWorld(t *testing.T) *classifierWorld {
	t.Helper()
	w := &classifierWorld{
		finishedResponseID: gatedResponseID,
		eventCount:         gatedEventCount,
		replyStatus:        http.StatusOK,
		replyBody:          completeReply(true),
		patchedBodies:      map[string]string{},
		movedTo:            map[string]string{},
	}

	board := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/tags"):
			out := []map[string]any{}
			for _, tag := range w.tags {
				out = append(out, map[string]any{"tag": tag})
			}
			writeJSON(rw, http.StatusOK, out)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/tags"):
			tag, _ := payload["tag"].(string)
			w.tagsAdded = append(w.tagsAdded, tag)
			writeJSON(rw, http.StatusCreated, map[string]any{})
		case r.Method == http.MethodDelete && strings.Contains(path, "/tags/"):
			w.tagsDeleted = append(w.tagsDeleted, path[strings.Index(path, "/tags/")+len("/tags/"):])
			writeJSON(rw, http.StatusOK, map[string]any{})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/entities/") && strings.HasSuffix(path, "/cards"):
			cards := w.cards
			if cards == nil {
				cards = []map[string]any{}
			}
			writeJSON(rw, http.StatusOK, cards)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/move"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/cards/"), "/move")
			column, _ := payload["column_id"].(string)
			w.movedTo[id] = column
			writeJSON(rw, http.StatusOK, map[string]any{})
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/api/cards/"):
			body, _ := payload["body"].(string)
			w.patchedBodies[strings.TrimPrefix(path, "/api/cards/")] = body
			writeJSON(rw, http.StatusOK, map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/links"):
			writeJSON(rw, http.StatusCreated, map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/cards"):
			title, _ := payload["title"].(string)
			w.createdTitles = append(w.createdTitles, title)
			writeJSON(rw, http.StatusCreated, map[string]any{"item": map[string]any{"id": "new-card"}})
		default:
			t.Errorf("stub board got an unexpected %s %s", r.Method, path)
			rw.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(board.Close)

	bridge := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages"):
			writeJSON(rw, http.StatusOK, w.sessionPage())
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oneshot"):
			w.modelCalls++
			var request struct {
				Prompt string `json:"prompt"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &request)
			w.sentPrompt = request.Prompt
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(w.replyStatus)
			_, _ = rw.Write([]byte(w.replyBody))
		default:
			t.Errorf("stub bridge got an unexpected %s %s", r.Method, r.URL.Path)
			rw.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(bridge.Close)

	w.kc = issuesclient.New(board.URL, "")
	w.kc.BoardID = "board-1"
	w.kc.ColumnsByName = map[string]string{
		"In Progress": "col-inprogress", "Blocked": "col-blocked",
		"Done": "col-done", "Failed": "col-failed",
	}
	w.cfg = config{
		BridgeURL:    bridge.URL,
		ModelRole:    "efficient",
		DigestBudget: digestCharBudget,
	}
	w.degraded = degradations.New("kanban-classifier")
	return w
}

// sessionPage is one turn a minute old: recent enough to pass the lookback, and
// ending in the finished response the once-per-response gate keys on — unless
// the test cleared finishedResponseID, in which case the turn has no result.
func (w *classifierWorld) sessionPage() map[string]any {
	now := time.Now().UTC()
	entryIDs := []string{"entry-prompt-gate"}
	entries := map[string]any{
		"entry-prompt-gate": map[string]any{"role": "user", "kind": "prompt", "text": "port the resolver", "ts": now.Add(-2 * time.Minute).Format(time.RFC3339)},
	}
	if w.finishedResponseID != "" {
		entryIDs = append(entryIDs, w.finishedResponseID)
		entries[w.finishedResponseID] = map[string]any{"role": "assistant", "kind": "result", "text": "ported", "ts": now.Add(-time.Minute).Format(time.RFC3339)}
	}
	return map[string]any{"model": map[string]any{
		"turns":     []any{map[string]any{"entryIds": entryIDs}},
		"entries":   entries,
		"validator": map[string]any{"eventCount": w.eventCount},
	}}
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *classifierWorld) run() bool {
	return w.runSession(gatedSession())
}

func (w *classifierWorld) runSession(sess session) bool {
	return processSession(context.Background(), w.cfg, w.kc, sess, w.degraded)
}

// gatedSession is deliberately NOT terminal: a terminal state would let the
// final-classified path color results that are about some other gate.
func gatedSession() session {
	return session{
		SessionID: "br_gate",
		State:     "idle",
		Harness:   "claudecode",
		Purpose:   "autoworker",
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- gate: final

func TestFinalClassifiedSessionIsNeverBilledAgain(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{tagFinalClassified}

	if ran := w.run(); ran {
		t.Error("processSession reported a classification for a final-classified session")
	}
	if w.modelCalls != 0 {
		t.Fatalf("a final-classified session reached the model %d time(s)", w.modelCalls)
	}
}

func TestNamingASessionExplicitlyOverridesTheFinalClassifiedSkip(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{tagFinalClassified}
	w.cfg.ExplicitSession = true

	if ran := w.run(); !ran {
		t.Error("an explicitly named session was skipped anyway")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1 — the gates must yield to an operator naming a session by id", w.modelCalls)
	}
}

// ------------------------------------------------- gate: no final output yet

// A turn still running has no final output; reading it now buys a call on a
// half-finished answer and a second call when it ends.
func TestASessionMidTurnIsNotBilled(t *testing.T) {
	w := newWorld(t)
	sess := gatedSession()
	sess.State = "model_generating"

	if ran := w.runSession(sess); ran {
		t.Error("processSession reported a classification for a session part-way through a turn")
	}
	if w.modelCalls != 0 {
		t.Fatalf("a session mid-turn reached the model %d time(s)", w.modelCalls)
	}
}

// A turn the person cut short, or one still running, carries no result entry,
// so there is no response to read and nothing to key a signature on.
func TestASessionWithNoFinishedResponseIsNotBilled(t *testing.T) {
	w := newWorld(t)
	w.finishedResponseID = ""

	if ran := w.run(); ran {
		t.Error("processSession reported a classification for a session with no finished response")
	}
	if w.modelCalls != 0 {
		t.Fatalf("a session with no finished response reached the model %d time(s)", w.modelCalls)
	}
}

// ---------------------------------------------------------- gate: do-not-track

// The deltas are written as literals on purpose. Seeding a fixture with
// doNotTrackReevalEvents itself would make the test move with the constant and
// pin nothing: a change from 2000 to 2 would keep it green.
func TestDoNotTrackSessionIsNotBilledUntilItGrowsByTwoThousandEvents(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{tagDoNotTrack, "kanban-check-sig:result=entry-result-older:events=3001"}
	w.eventCount = 5000 // Δ = 1999, one short

	if ran := w.run(); ran {
		t.Error("processSession reported a classification it did not make")
	}
	if w.modelCalls != 0 {
		t.Fatalf("a do-not-track session 1999 events on reached the model %d time(s)", w.modelCalls)
	}
}

func TestDoNotTrackSessionIsReEvaluatedOnceItGrowsByTwoThousandEvents(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{tagDoNotTrack, "kanban-check-sig:result=entry-result-older:events=3000"}
	w.eventCount = 5000 // Δ = 2000, exactly the threshold

	if !w.run() {
		t.Error("a do-not-track session past the delta was still skipped")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1 — a grown session must be re-evaluated", w.modelCalls)
	}
}

// A do-not-track signature from before event counts has nothing to measure a
// delta against. Measuring from zero would re-open every such session at once;
// the gate instead records today's count as the baseline and stays shut.
func TestADoNotTrackSessionWithNoRecordedEventCountGetsABaselineNotAModelCall(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{tagDoNotTrack, preResponseSigTag}

	if ran := w.run(); ran {
		t.Error("processSession reported a classification it did not make")
	}
	if w.modelCalls != 0 {
		t.Fatalf("a do-not-track session with a pre-event-count signature reached the model %d time(s)", w.modelCalls)
	}
	if !contains(w.tagsAdded, gatedSignature) {
		t.Fatalf("no baseline signature was written, so the next tick has nothing to measure from either: added %v", w.tagsAdded)
	}
}

func TestNamingASessionExplicitlyOverridesTheDoNotTrackSkip(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{tagDoNotTrack, "kanban-check-sig:result=entry-result-older:events=4990"}
	w.cfg.ExplicitSession = true

	if !w.run() {
		t.Error("an explicitly named do-not-track session was skipped anyway")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", w.modelCalls)
	}
}

// ------------------------------------------------------ gate: check signature

func TestAResponseAlreadyClassifiedIsNotBilledAgain(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{gatedSignature}

	if ran := w.run(); ran {
		t.Error("processSession reported a classification for a response it already read")
	}
	if w.modelCalls != 0 {
		t.Fatalf("an already-classified response reached the model %d time(s)", w.modelCalls)
	}
}

func TestANewerFinishedResponseIsBilledAgain(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{supersededSigTag}

	if !w.run() {
		t.Error("a session with a newer finished response was skipped as already read")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", w.modelCalls)
	}
}

func TestNamingASessionExplicitlyOverridesTheAlreadyClassifiedSkip(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{gatedSignature}
	w.cfg.ExplicitSession = true

	if !w.run() {
		t.Error("an explicitly named, already-classified session was skipped anyway")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", w.modelCalls)
	}
}

// A session the classifier has never seen has no signature at all, and must be
// classified. This is the case that tells an over-eager gate from a working one:
// a gate that matches everything also skips first contact.
func TestASessionWithNoPriorSignatureIsClassified(t *testing.T) {
	w := newWorld(t)

	if !w.run() {
		t.Error("a session with no prior signature was skipped")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", w.modelCalls)
	}
	if !strings.Contains(w.sentPrompt, gatedSession().SessionID) {
		t.Errorf("the model was not sent this session's digest: %q", w.sentPrompt)
	}
}

// ------------------------------------------------------------ what a run records

func TestAClassifiedSessionRecordsTheResponseItRead(t *testing.T) {
	w := newWorld(t)

	w.run()

	if !contains(w.tagsAdded, gatedSignature) {
		t.Fatalf("want %q among the tags written, got %v", gatedSignature, w.tagsAdded)
	}
}

// The signature is the classifier's whole memory of a session, so a stale one
// left behind is a second answer to "which response was last read".
func TestWritingASignatureRemovesTheOneItSupersedes(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{supersededSigTag}

	w.run()

	if !contains(w.tagsDeleted, supersededSigTag) {
		t.Errorf("the superseded signature %q was left in place; deleted %v", supersededSigTag, w.tagsDeleted)
	}
}

// An explicit re-run of an already-read response writes the same signature it
// found. Deleting it first and then failing the add would leave the session with
// no signature, and the next tick would pay for it again.
func TestWritingASignatureDoesNotDeleteTheOneItIsWriting(t *testing.T) {
	w := newWorld(t)
	w.tags = []string{gatedSignature}
	w.cfg.ExplicitSession = true

	w.run()

	if contains(w.tagsDeleted, gatedSignature) {
		t.Errorf("the signature being written was itself deleted: %v", w.tagsDeleted)
	}
}

func TestATerminalSessionIsMarkedFinalClassified(t *testing.T) {
	for _, state := range []string{terminalStateCompleted, terminalStateError} {
		t.Run(state, func(t *testing.T) {
			w := newWorld(t)
			sess := gatedSession()
			sess.State = state

			if !w.runSession(sess) {
				t.Fatal("a terminal session was not classified")
			}
			if !contains(w.tagsAdded, tagFinalClassified) {
				t.Fatalf("state=%s was not marked final-classified; tags written: %v", state, w.tagsAdded)
			}
		})
	}
}

func TestANonTerminalSessionIsNotMarkedFinalClassified(t *testing.T) {
	w := newWorld(t)

	w.run()

	if contains(w.tagsAdded, tagFinalClassified) {
		t.Fatalf("an idle session was retired as final-classified: %v", w.tagsAdded)
	}
}

func TestAnUntrackableSessionIsTaggedDoNotTrack(t *testing.T) {
	w := newWorld(t)
	w.replyBody = completeReply(false)

	if !w.run() {
		t.Error("an untrackable verdict is still a model call that was made")
	}
	if !contains(w.tagsAdded, tagDoNotTrack) {
		t.Fatalf("an untrackable session was not tagged do-not-track, so the next response re-bills it: %v", w.tagsAdded)
	}
	if !contains(w.tagsAdded, gatedSignature) {
		t.Errorf("an untrackable session recorded no signature, so the do-not-track delta has no baseline: %v", w.tagsAdded)
	}
	if len(w.createdTitles) != 0 {
		t.Errorf("an untrackable session filed cards: %v", w.createdTitles)
	}
}

// ------------------------------------------------------------------ the dry run

// -dry-run promises the board is not touched. The classifier still makes the
// call, so the promise is specifically about writes, not about spend.
func TestDryRunWritesNothingAtAll(t *testing.T) {
	w := newWorld(t)
	w.cfg.DryRun = true
	w.tags = []string{supersededSigTag}
	w.replyBody = completeReply(true, NewRec{Title: "Port the resolver", State: "in_progress", Summary: "s"})

	if !w.run() {
		t.Error("a dry run still made the model call and must report it")
	}
	if w.modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", w.modelCalls)
	}
	if len(w.createdTitles) != 0 || len(w.tagsAdded) != 0 || len(w.tagsDeleted) != 0 {
		t.Fatalf("-dry-run wrote to the board: created=%v tagsAdded=%v tagsDeleted=%v",
			w.createdTitles, w.tagsAdded, w.tagsDeleted)
	}
}

// ------------------------------------------------------- the truncated report

// A truncated report's missing existing_cards are indistinguishable from "no
// opinion", so acting on any part of it is worse than acting on none.
func TestATruncatedReportReachesTheBoardNowhere(t *testing.T) {
	w := newWorld(t)
	w.cards = []map[string]any{{"card_id": "card-1", "item": map[string]any{"title": "Existing work", "status": "open"}}}
	w.replyBody = truncatedReply()

	if !w.run() {
		t.Error("a truncated classification was still a model call and must be reported as a run")
	}
	if len(w.createdTitles) != 0 {
		t.Errorf("a truncated report filed cards: %v", w.createdTitles)
	}
	if len(w.patchedBodies) != 0 || len(w.movedTo) != 0 {
		t.Errorf("a truncated report moved or rewrote cards: moved=%v patched=%v", w.movedTo, w.patchedBodies)
	}
}

// The input is deterministic, so a retry truncates identically and pays for it
// every tick. The signature is what stops that.
func TestATruncatedReportStillRecordsItsSignatureSoTheTickIsNotRepaid(t *testing.T) {
	w := newWorld(t)
	w.replyBody = truncatedReply()

	w.run()

	if !contains(w.tagsAdded, gatedSignature) {
		t.Fatalf("no signature was recorded for a truncated report, so the identical digest is re-billed next tick: %v", w.tagsAdded)
	}
}

func TestDryRunDoesNotRecordTheSignatureOfATruncatedReport(t *testing.T) {
	w := newWorld(t)
	w.cfg.DryRun = true
	w.replyBody = truncatedReply()

	w.run()

	if len(w.tagsAdded) != 0 {
		t.Fatalf("-dry-run wrote a signature tag: %v", w.tagsAdded)
	}
}

// A transport or model failure is the opposite case: nothing was learned, so the
// signature must NOT move or the session is never retried.
func TestAFailedClassificationLeavesTheSignatureAloneForRetry(t *testing.T) {
	w := newWorld(t)
	w.replyBody = `{"stop_reason":"end_turn","text":"I would rather not."}` // no parsed report

	if w.run() {
		t.Error("a failed classification was reported as a completed run")
	}
	if len(w.tagsAdded) != 0 || len(w.tagsDeleted) != 0 {
		t.Fatalf("a failed classification moved the signature, so the retry never happens: added=%v deleted=%v",
			w.tagsAdded, w.tagsDeleted)
	}
}
