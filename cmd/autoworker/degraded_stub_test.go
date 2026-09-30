package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The stubs below stand in for usage-store, kanban-store, noteboard and
// llm-bridge-server. Each fault field is a status code: zero means "answer
// normally", non-zero means answer with that status, which is how every failure
// this suite is about arrives in production — the service answering, just not
// with the status the client wanted.
//
// The bus is not among them. It is the one service a test cannot stand up
// without dispatching a real agent session, so it crosses the workerPublisher
// seam instead and every test states what publishing did.

// captureLog collects everything the run under it writes to the standard logger,
// which is where degradations.Report lands and therefore the only place a person
// reading scheduler run history sees a shortfall.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	priorOut, priorFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(priorOut); log.SetFlags(priorFlags) })
	fn()
	return buf.String()
}

// shortfallLine returns the single `run degraded:` line out of a captured log,
// and fails the test if there is not exactly one.
//
// It exists because asserting with strings.Contains over the WHOLE log is
// answered by whatever else happened to print — and this command logs a line
// naming the same provider or card immediately before every Record it makes, so
// a shortfall report that dropped the name entirely would stay green. That trap
// was measured on cmd/spend-ceiling-guard and again six times over on
// cmd/kanban-curator, both on 2026-08-29.
func shortfallLine(t *testing.T, out string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "run degraded:") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one `run degraded:` line, got %d in:\n%s", len(found), out)
	}
	return found[0]
}

func requireNoShortfall(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "run degraded:") {
		t.Fatalf("a deliberate skip was reported as a shortfall:\n%s", out)
	}
}

func writeStubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- usage-store

// stubUsage answers GET /api/usage/limits. The zero value answers a snapshot
// that fires anthropic: fresh, with a seven_day window at 0% used against a
// target well above it.
type stubUsage struct {
	t      *testing.T
	body   map[string]any
	fail   int
	server *httptest.Server
}

// firingLimits is a fresh snapshot whose anthropic window is far enough under
// target to open every gate. resets_at is six days out, which puts the elapsed
// fraction near the top of the curve where the target is high.
func firingLimits() map[string]any {
	resets := time.Now().Add(24 * time.Hour).Unix()
	return map[string]any{
		"anthropic": map[string]any{
			"provider":    "anthropic",
			"snapshot_at": time.Now().Unix(),
			"windows": map[string]any{
				"seven_day": map[string]any{"used_percent": 0.0, "resets_at": resets},
			},
		},
	}
}

func newStubUsage(t *testing.T, body map[string]any) *stubUsage {
	u := &stubUsage{t: t, body: body}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

func (u *stubUsage) URL() string { return u.server.URL }

func (u *stubUsage) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/usage/limits" {
		u.t.Errorf("stub usage-store: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", 500)
		return
	}
	if u.fail != 0 {
		http.Error(w, "usage-store unavailable", u.fail)
		return
	}
	writeStubJSON(w, 200, u.body)
}

// ---------------------------------------------------------------- kanban-store

const (
	stubQueueBoardID = "board-queue"
	stubRunsBoardID  = "board-runs"
	stubColQueued    = "col-queued"
	stubColInProgQ   = "col-inprogress-queue"
	stubColInProgR   = "col-inprogress-runs"
)

// stubQueueCard is one card in the Queued column as autoworker meets it.
type stubQueueCard struct {
	cardID   string
	title    string
	body     string
	noItem   bool     // the placement resolves to no noteboard item
	heldAt   string   // non-empty parks the card behind the hold gate
	agents   []string // "agent" entity links
	repos    []string // "repo" entity links
	ceiling  float64  // auto_hold_at_usd on the noteboard item; 0 = no ceiling
	sessions []string // "session" links, which is what the spend ceiling counts
}

type stubKanban struct {
	t     *testing.T
	cards []stubQueueCard

	failQueueBoards int // status for the board lookup that resolves the queue
	failBoardView   int // status for GET /api/boards/{queue}/cards — the queue read itself
	failCreateCard  int // status for POST /api/boards/{id}/cards
	failLink        int // status for POST /api/cards/{id}/links
	failMove        int // status for POST /api/cards/{id}/move
	failHold        int // status for POST /api/cards/{id}/hold

	created []string // titles of cards created on the "Agent runs" board
	moved   map[string]string
	held    []string
	server  *httptest.Server
}

func newStubKanban(t *testing.T, cards ...stubQueueCard) *stubKanban {
	k := &stubKanban{t: t, cards: cards, moved: map[string]string{}}
	k.server = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.server.Close)
	return k
}

func (k *stubKanban) URL() string { return k.server.URL }

func (k *stubKanban) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/boards" && r.Method == "GET":
		if k.failQueueBoards != 0 {
			http.Error(w, "boards unavailable", k.failQueueBoards)
			return
		}
		writeStubJSON(w, 200, []map[string]string{
			{"id": stubQueueBoardID, "name": autoworkerQueueBoard},
			{"id": stubRunsBoardID, "name": kanbanBoardName},
		})

	case p == "/api/boards/"+stubQueueBoardID+"/columns" && r.Method == "GET":
		writeStubJSON(w, 200, stubColumns(stubQueueBoardID, stubColQueued, stubColInProgQ))

	case p == "/api/boards/"+stubRunsBoardID+"/columns" && r.Method == "GET":
		writeStubJSON(w, 200, stubColumns(stubRunsBoardID, "col-queued-runs", stubColInProgR))

	case p == "/api/boards/"+stubQueueBoardID+"/cards" && r.Method == "GET":
		if k.failBoardView != 0 {
			http.Error(w, "board view unavailable", k.failBoardView)
			return
		}
		writeStubJSON(w, 200, k.queueView())

	case p == "/api/boards/"+stubRunsBoardID+"/cards" && r.Method == "POST":
		if k.failCreateCard != 0 {
			http.Error(w, "create card refused", k.failCreateCard)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		title, _ := req["title"].(string)
		k.created = append(k.created, title)
		writeStubJSON(w, 201, map[string]any{"item": map[string]any{"id": "run-card-1"}})

	case strings.HasSuffix(p, "/links") && r.Method == "GET":
		cardID := strings.TrimSuffix(strings.TrimPrefix(p, "/api/cards/"), "/links")
		links := []map[string]any{}
		for _, c := range k.cards {
			if c.cardID != cardID {
				continue
			}
			for _, s := range c.sessions {
				links = append(links, map[string]any{"card_id": c.cardID, "entity_type": "session", "entity_ref": s})
			}
		}
		writeStubJSON(w, 200, links)

	case strings.HasSuffix(p, "/links") && r.Method == "POST":
		if k.failLink != 0 {
			http.Error(w, "link refused", k.failLink)
			return
		}
		writeStubJSON(w, 201, map[string]string{"status": "ok"})

	case strings.HasSuffix(p, "/move") && r.Method == "POST":
		if k.failMove != 0 {
			http.Error(w, "move refused", k.failMove)
			return
		}
		var req struct {
			ColumnID string `json:"column_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		k.moved[strings.TrimSuffix(strings.TrimPrefix(p, "/api/cards/"), "/move")] = req.ColumnID
		writeStubJSON(w, 200, map[string]string{"status": "ok"})

	case strings.HasSuffix(p, "/hold") && r.Method == "POST":
		if k.failHold != 0 {
			http.Error(w, "hold refused", k.failHold)
			return
		}
		k.held = append(k.held, strings.TrimSuffix(strings.TrimPrefix(p, "/api/cards/"), "/hold"))
		writeStubJSON(w, 200, map[string]string{"status": "ok"})

	case strings.HasPrefix(p, "/api/cards/") && r.Method == "PATCH":
		writeStubJSON(w, 200, map[string]string{"status": "ok"})

	default:
		k.t.Errorf("stub kanban-store: unexpected %s %s", r.Method, p)
		http.Error(w, "unexpected", 500)
	}
}

func stubColumns(boardID, queuedID, inProgID string) []map[string]any {
	out := []map[string]any{
		{"id": queuedID, "board_id": boardID, "name": "Queued"},
		{"id": inProgID, "board_id": boardID, "name": "In Progress"},
	}
	for _, n := range []string{"Blocked", "Done", "Failed"} {
		out = append(out, map[string]any{"id": boardID + "-" + n, "board_id": boardID, "name": n})
	}
	return out
}

func (k *stubKanban) queueView() map[string]any {
	var cards []map[string]any
	for _, c := range k.cards {
		cv := map[string]any{
			"placement": map[string]any{
				"card_id": c.cardID, "board_id": stubQueueBoardID, "column_id": stubColQueued,
			},
		}
		if !c.noItem {
			item := map[string]any{"id": c.cardID, "title": c.title, "body": c.body}
			if c.heldAt != "" {
				item["held_at"] = c.heldAt
				item["hold_reason"] = "parked by the user"
			}
			if c.ceiling > 0 {
				item["auto_hold_at_usd"] = c.ceiling
			}
			cv["item"] = item
		}
		var links []map[string]any
		for _, a := range c.agents {
			links = append(links, map[string]any{"card_id": c.cardID, "entity_type": "agent", "entity_ref": a})
		}
		for _, rp := range c.repos {
			links = append(links, map[string]any{"card_id": c.cardID, "entity_type": "repo", "entity_ref": rp})
		}
		cv["links"] = links
		cards = append(cards, cv)
	}
	col := func(id, name string, cs []map[string]any) map[string]any {
		return map[string]any{
			"column": map[string]any{"id": id, "board_id": stubQueueBoardID, "name": name},
			"cards":  cs,
		}
	}
	return map[string]any{
		"board": map[string]any{"id": stubQueueBoardID, "name": autoworkerQueueBoard},
		"columns": []map[string]any{
			col(stubColQueued, "Queued", cards),
			col(stubColInProgQ, "In Progress", nil),
		},
	}
}

// ------------------------------------------------- noteboard and llm-bridge

// stubSpend answers the two reads behind the per-card spend ceiling: noteboard's
// item list and the bridge's session aggregates. Both answer empty by default,
// which is "no ceilings anywhere and nothing has cost anything".
type stubSpend struct {
	t      *testing.T
	items  []map[string]any
	aggs   []map[string]any
	server *httptest.Server
}

func newStubSpend(t *testing.T) *stubSpend {
	s := &stubSpend{t: t}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubSpend) URL() string { return s.server.URL }

func (s *stubSpend) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/items"):
		// A bare array, as noteboard sends it. This used to be wrapped in
		// {"items": …}, which fetchNoteboardItems cannot decode, so every tick
		// through this stub read the spend tree as unreadable.
		items := s.items
		if items == nil {
			items = []map[string]any{}
		}
		writeStubJSON(w, 200, items)
	case r.URL.Path == "/sessions/aggregates":
		writeStubJSON(w, 200, s.aggs)
	default:
		s.t.Errorf("stub spend: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", 500)
	}
}

// ------------------------------------------------------------------- harness

// tickConfig assembles a config pointed at the stubs, with a state file in a
// temp dir so no test touches ~/.config/autoworker/state.json.
func tickConfig(t *testing.T, usage, kanban, spend string) config {
	t.Helper()
	return config{
		slackPercentPoints: 15,
		prompt:             "the default discovery prompt",
		timezone:           "America/Los_Angeles",
		natsURL:            "nats://unreachable.invalid:4222",
		kanbanStoreURL:     kanban,
		llmBridgeURL:       spend,
		noteboardURL:       spend,
		usageStoreURL:      usage,
		schedulerURL:       "http://unreachable.invalid",
		statePath:          filepath.Join(t.TempDir(), "state.json"),
	}
}

// recordingPublisher is a workerPublisher that succeeds and remembers what it
// was asked to dispatch.
type recordingPublisher struct {
	calls   int
	prompts []string
	budgets []float64
}

func (p *recordingPublisher) publish(_ providerSpec, _ string, prompt string, maxBudgetUSD float64, _, _, _ string) (string, error) {
	p.calls++
	p.prompts = append(p.prompts, prompt)
	p.budgets = append(p.budgets, maxBudgetUSD)
	return "autoworker-anthropic-1", nil
}

// newStubEmptyBoards is a kanban-store with no boards at all: a fresh install
// before anything has bootstrapped the queue.
func newStubEmptyBoards(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/boards" && r.Method == "GET" {
			writeStubJSON(w, 200, []map[string]string{})
			return
		}
		t.Errorf("stub empty kanban-store: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", 500)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// defaultLaneModelRole is a laneModelRoleReader for a tick test that is about
// something other than the model role: every lane runs on default.
func defaultLaneModelRole(providerSpec) (string, error) { return "default", nil }
