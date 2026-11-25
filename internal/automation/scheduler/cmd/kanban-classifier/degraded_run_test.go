package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
)

// The body the Anthropic API really returned to this job on 2026-08-29, when
// the account's credit ran out at 15:15 and the classifier stopped classifying.
// Kept verbatim: this is the input that produced 40 of 40 green runs, so it is
// the input the fix has to be scored on. The model call now goes through
// llm-bridge-server's oneshot route, so the stub relays it as that route's
// non-200 answer.
const creditExhausted400 = `{"type":"error","error":{"type":"invalid_request_error",` +
	`"message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`

// bridgeAndBoard stands up the two services processSession talks to, with every
// route answering the happy case. Each test breaks exactly one of them, so the
// shortfall it asserts can only have come from the route it broke.
type bridgeAndBoard struct {
	// moveStatus is the status the card-move route answers with. 404 reproduces
	// the live defect found in job 14's run history.
	moveStatus int
	// tagWriteStatus is the status the entity-tag write answers with.
	tagWriteStatus int
	// tagDeleteStatus is the status the entity-tag delete answers with.
	tagDeleteStatus int
	// tagReadStatus is the status the entity-tag read answers with. The board is
	// a separate service and this is the first thing processSession asks it.
	tagReadStatus int
	// tags is what ListEntityTags returns, which is what the dedupe gate reads.
	tags []string
	// cards is what ListCardsForEntity returns.
	cards []map[string]any
	// modelStatus and modelBody are what the oneshot route answers with.
	modelStatus int
	modelBody   string
	// modelCalls counts requests that reached the stub oneshot route.
	modelCalls int
}

func (b *bridgeAndBoard) start(t *testing.T) (config, *kanbanclient.Client) {
	t.Helper()
	if b.moveStatus == 0 {
		b.moveStatus = http.StatusOK
	}
	if b.modelStatus == 0 {
		b.answerModelCalls(http.StatusOK, `{"stop_reason":"end_turn","parsed":{"trackable":false,"session_summary":"nothing to track"}}`)
	}
	if b.tagWriteStatus == 0 {
		b.tagWriteStatus = http.StatusCreated
	}
	if b.tagDeleteStatus == 0 {
		b.tagDeleteStatus = http.StatusOK
	}
	if b.tagReadStatus == 0 {
		b.tagReadStatus = http.StatusOK
	}

	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/tags"):
			if b.tagReadStatus != http.StatusOK {
				w.WriteHeader(b.tagReadStatus)
				_, _ = w.Write([]byte(`{"error":"board unavailable"}`))
				return
			}
			out := []map[string]any{}
			for _, tag := range b.tags {
				out = append(out, map[string]any{"tag": tag})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/tags"):
			w.WriteHeader(b.tagWriteStatus)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(b.tagDeleteStatus)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/cards"):
			if b.cards == nil {
				b.cards = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(b.cards)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/move"):
			w.WriteHeader(b.moveStatus)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("stub board got an unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(board.Close)

	// llm-bridge-server serves both the session page and the oneshot call the
	// model goes through, so one stub answers both.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages"):
			_ = json.NewEncoder(w).Encode(finishedSessionPage())
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oneshot"):
			b.modelCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(b.modelStatus)
			_, _ = w.Write([]byte(b.modelBody))
		default:
			t.Errorf("stub bridge got an unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(bridge.Close)

	kc := kanbanclient.New(board.URL, "")
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{
		"In Progress": "col-inprogress", "Blocked": "col-blocked",
		"Done": "col-done", "Failed": "col-failed",
	}
	return config{
		BridgeURL:    bridge.URL,
		ModelRole:    "efficient",
		DigestBudget: 4000,
		MaxClassify:  10,
	}, kc
}

// finishedResponseID and finishedEventCount are what the stub session page
// carries, so a test can pre-write the signature processSession will compute.
const (
	finishedResponseID = "entry-result-1"
	finishedEventCount = 3
)

// finishedSessionPage is one turn that ended on its own terms a minute ago:
// recent enough to pass the lookback, and carrying the finished response the
// once-per-response gate keys on.
func finishedSessionPage() map[string]any {
	now := time.Now().UTC()
	return map[string]any{"model": map[string]any{
		"turns": []any{map[string]any{"entryIds": []string{"entry-prompt-1", finishedResponseID}}},
		"entries": map[string]any{
			"entry-prompt-1":   map[string]any{"role": "user", "kind": "prompt", "text": "port the resolver", "ts": now.Add(-2 * time.Minute).Format(time.RFC3339)},
			finishedResponseID: map[string]any{"role": "assistant", "kind": "result", "text": "ported", "ts": now.Add(-time.Minute).Format(time.RFC3339)},
		},
		"validator": map[string]any{"eventCount": finishedEventCount},
	}}
}

// answerModelCalls sets the status and raw body the stub oneshot route answers
// with.
func (b *bridgeAndBoard) answerModelCalls(status int, body string) {
	b.modelStatus = status
	b.modelBody = body
}

// TestARefusedModelCallDegradesTheRun is card b4130d06 itself.
//
// On 2026-08-29 the credit balance ran out at 15:15. Job 54 (email-classifier)
// went red on the first tick; job 14 (this binary) reported `success` on 40 of
// 40 runs, because processSession logged the 400 and returned false, and a run
// made only of false printed `classified=0` and exited 0. Both jobs printed a
// zero in their last line and only one of them was findable.
func TestARefusedModelCallDegradesTheRun(t *testing.T) {
	stub := &bridgeAndBoard{}
	cfg, kc := stub.start(t)
	stub.answerModelCalls(http.StatusBadGateway, creditExhausted400)
	degraded := degradations.New("classifier")

	ran := processSession(context.Background(), cfg, kc, testSession(), degraded)

	if ran {
		t.Error("a session the model refused was counted as classified")
	}
	if stub.modelCalls != 1 {
		t.Fatalf("the model was called %d times, want 1 — the fixture did not reach classify", stub.modelCalls)
	}
	if !degraded.HasShortfall() {
		t.Fatal("the run that the credit outage emptied did not record a shortfall — this is the whole defect")
	}
	if got := degraded.ExitCode(); got != 1 {
		t.Errorf("the run exited %d, want 1 so the scheduler records `error` instead of `success`", got)
	}
	if joined := strings.Join(degraded.Reasons(), "; "); !strings.Contains(joined, testSession().SessionID) {
		t.Errorf("the summary %q does not name the session that went unclassified", joined)
	}
}

// TestACardThatCannotMoveDegradesTheRun is the other half, and it is live right
// now rather than hypothetical: measured on job 14's run history 2026-08-29,
// card b6765116 answered `404 Not Found` to every move on 8 of the last 60
// ticks. Those runs recorded `success` while the board kept showing a card in
// the column it was already in.
//
// It is the more dangerous half. An unclassified session at least looks
// unfiled; a card that failed to move looks current.
func TestACardThatCannotMoveDegradesTheRun(t *testing.T) {
	stub := &bridgeAndBoard{moveStatus: http.StatusNotFound}
	_, kc := stub.start(t)
	degraded := degradations.New("classifier")

	applyExisting(kc, testSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "b6765116", State: "done", Summary: "finished the resolver"},
	}}, degraded)

	if !degraded.HasShortfall() {
		t.Fatal("a card that answered 404 to its move did not register as a shortfall")
	}
	if got := degraded.ExitCode(); got != 1 {
		t.Errorf("the run exited %d, want 1", got)
	}
	if joined := strings.Join(degraded.Reasons(), "; "); !strings.Contains(joined, "b6765116") {
		t.Errorf("the summary %q does not name the card that stayed put", joined)
	}
}

// TestAnUnwritableSignatureDegradesTheRun — the signature IS the dedupe gate. A
// tag that does not land means this session is classified again on the next
// tick, and every tick after it, at the price of a model call each time. The
// only outward sign is the bill.
func TestAnUnwritableSignatureDegradesTheRun(t *testing.T) {
	stub := &bridgeAndBoard{tagWriteStatus: http.StatusInternalServerError}
	_, kc := stub.start(t)
	degraded := degradations.New("classifier")

	writeSig(kc, testSession(), map[string]bool{}, buildSig(finishedResponseID, finishedEventCount), degraded)

	if !degraded.HasShortfall() {
		t.Fatal("a check signature that was not written did not register as a shortfall")
	}
	if joined := strings.Join(degraded.Reasons(), "; "); !strings.Contains(joined, "re-classified") {
		t.Errorf("the summary %q does not say what the lost signature will cost", joined)
	}
}

// TestASkippedSessionIsNotAShortfall is the assertion that keeps the signal
// worth reading. Most candidates on a normal tick are skipped on purpose — the
// signature has not moved, so there is nothing new to classify — and a tick made
// entirely of those did its whole job. If deliberate skips recorded shortfalls,
// job 14 would be red every 15 minutes and the red would mean nothing, which is
// the same defect as the green it replaces, pointing the other way.
func TestASkippedSessionIsNotAShortfall(t *testing.T) {
	sess := testSession()
	// The signature processSession will compute for this session, pre-written as
	// the prior tag, so the dedupe gate closes before anything can fail.
	priorSig := buildSig(finishedResponseID, finishedEventCount)

	stub := &bridgeAndBoard{tags: []string{priorSig}}
	cfg, kc := stub.start(t)
	stub.answerModelCalls(http.StatusBadGateway, creditExhausted400)
	degraded := degradations.New("classifier")

	ran := processSession(context.Background(), cfg, kc, sess, degraded)

	if ran {
		t.Error("an unchanged session was classified anyway")
	}
	if stub.modelCalls != 0 {
		t.Fatalf("the model was called %d times for an unchanged session", stub.modelCalls)
	}
	if degraded.HasShortfall() {
		t.Fatalf("a deliberate skip was recorded as a shortfall: %v", degraded.Reasons())
	}
	if got := degraded.ExitCode(); got != 0 {
		t.Errorf("a tick that skipped everything on purpose exited %d, want 0", got)
	}
}

// TestAnUnreadableSessionDegradesTheRun — the bridge is a separate service and
// can be down on its own. A candidate whose messages cannot be read is not
// classified, and before this it was not classified in silence.
func TestAnUnreadableSessionDegradesTheRun(t *testing.T) {
	stub := &bridgeAndBoard{}
	cfg, kc := stub.start(t)
	// Point the bridge at a closed port rather than at a stub returning 500:
	// the outage this models is the service being gone, not answering badly.
	cfg.BridgeURL = "http://127.0.0.1:1"
	degraded := degradations.New("classifier")

	if ran := processSession(context.Background(), cfg, kc, testSession(), degraded); ran {
		t.Error("a session whose messages could not be read was counted as classified")
	}
	if !degraded.HasShortfall() {
		t.Fatal("an unreachable bridge did not register as a shortfall")
	}
}

// TestAnUnreadableBoardDegradesTheRun — the board is the other separate service,
// and its tags are the first thing processSession reads. A candidate whose tags
// cannot be read is skipped before the dedupe gate is even consulted, so it
// looks from the outside exactly like a candidate with nothing new to classify.
//
// This case was added because the scorer said so: the sabotage that deletes this
// site's Record call came back MISSED against the five cases written first, and
// a shortfall no case reaches is a shortfall the next edit can delete in silence.
func TestAnUnreadableBoardDegradesTheRun(t *testing.T) {
	stub := &bridgeAndBoard{tagReadStatus: http.StatusInternalServerError}
	cfg, kc := stub.start(t)
	stub.answerModelCalls(http.StatusBadGateway, creditExhausted400)
	degraded := degradations.New("classifier")

	if ran := processSession(context.Background(), cfg, kc, testSession(), degraded); ran {
		t.Error("a session whose tags could not be read was counted as classified")
	}
	if stub.modelCalls != 0 {
		t.Fatalf("the model was called %d times after the board read failed", stub.modelCalls)
	}
	if !degraded.HasShortfall() {
		t.Fatal("an unreadable board did not register as a shortfall")
	}
	if joined := strings.Join(degraded.Reasons(), "; "); !strings.Contains(joined, testSession().SessionID) {
		t.Errorf("the summary %q does not name the session that went unclassified", joined)
	}
}
