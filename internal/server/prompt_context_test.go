package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeKanbanCards serves GET /api/cards/{id} for the cards given, each with
// its noteboard item's tags, and 404 for any other card.
func fakeKanbanCards(t *testing.T, tagsByCard map[string][]string) *kanbanclient.Client {
	t.Helper()
	kanban := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cardID, ok := strings.CutPrefix(r.URL.Path, "/api/cards/")
		tags, known := tagsByCard[cardID]
		if !ok || !known {
			http.Error(w, `{"error":"card not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"card_id": cardID, "item": map[string]any{"id": cardID, "tags": tags}})
	}))
	t.Cleanup(kanban.Close)
	return kanbanclient.New(kanban.URL)
}

// contextTestAgentStore holds a native_file Claude Code, an inject Codex, and
// context collections for every directory and for /work/northwind.
func contextTestAgentStore(t *testing.T) *agentstore.Store {
	t.Helper()
	as, err := agentstore.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { as.Close() })
	for _, delivery := range []agentstore.PromptHarnessDelivery{
		{Harness: string(msg.HarnessClaudeCode), Delivery: agentstore.PromptDeliveryNativeFile, NativeRelativePath: "CLAUDE.md"},
		{Harness: string(msg.HarnessCodex), Delivery: agentstore.PromptDeliveryInject},
	} {
		if _, err := as.SetPromptHarnessDelivery(delivery); err != nil {
			t.Fatal(err)
		}
	}
	everywhere, err := as.EnsurePromptCollection("context", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	northwind, err := as.EnsurePromptCollection("context", "/work/northwind", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []struct {
		collection int64
		heading    string
		body       string
		tags       []string
	}{
		{everywhere.ID, "## Refunds", "Refunds go through the ledger.", []string{"billing"}},
		{northwind.ID, "## Northwind billing", "Northwind bills monthly.", []string{"billing"}},
	} {
		if _, err := as.CreatePromptSection(section.collection, &agentstore.PromptSection{Heading: section.heading, Body: section.body, Tags: section.tags, Enabled: true}, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	return as
}

func harnessConfigString(t *testing.T, sess *store.Session, key string) (string, bool) {
	t.Helper()
	var cfg map[string]json.RawMessage
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			t.Fatal(err)
		}
	}
	raw, ok := cfg[key]
	if !ok {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatal(err)
	}
	return text, true
}

// A card's tags select the context sections a spawn injects, and the key they
// go in follows the harness's delivery: Claude Code keeps its own system
// prompt and gets the text appended; Codex gets it as its whole prompt.
func TestInjectedContextFollowsTheCardTagsAndTheDelivery(t *testing.T) {
	srv := &Server{cfg: &config.Config{}, agentStore: contextTestAgentStore(t), kanbanClient: fakeKanbanCards(t, map[string][]string{"card-billing": {"billing"}, "card-other": {"ops"}})}
	ctx := context.Background()

	claudeCode := &store.Session{SessionID: "br_cc", Harness: msg.HarnessClaudeCode, CardID: "card-billing"}
	srv.injectPromptContext(ctx, claudeCode, &msg.Instance{})
	if _, ok := harnessConfigString(t, claudeCode, "system_prompt"); ok {
		t.Fatal("Claude Code was given a system_prompt, which replaces its default prompt")
	}
	appended, _ := harnessConfigString(t, claudeCode, "append_system_prompt")
	if !strings.Contains(appended, "Refunds go through the ledger.") {
		t.Fatalf("append_system_prompt = %q, want the billing section", appended)
	}
	if strings.Contains(appended, "Northwind bills monthly.") {
		t.Fatal("a section rooted at /work/northwind reached a session with no working directory")
	}

	codex := &store.Session{SessionID: "br_codex", Harness: msg.HarnessCodex, CardID: "card-billing"}
	srv.injectPromptContext(ctx, codex, &msg.Instance{})
	if prompt, _ := harnessConfigString(t, codex, "system_prompt"); !strings.Contains(prompt, "Refunds go through the ledger.") {
		t.Fatalf("codex system_prompt = %q, want the billing section", prompt)
	}

	otherCard := &store.Session{SessionID: "br_other", Harness: msg.HarnessClaudeCode, CardID: "card-other"}
	srv.injectPromptContext(ctx, otherCard, &msg.Instance{})
	if len(otherCard.HarnessConfig) != 0 {
		t.Fatalf("a card without the billing tag was given %s", otherCard.HarnessConfig)
	}

	chosen := &store.Session{SessionID: "br_chosen", Harness: msg.HarnessClaudeCode, CardID: "card-billing", HarnessConfig: json.RawMessage(`{"system_prompt":"classify this"}`)}
	srv.injectPromptContext(ctx, chosen, &msg.Instance{})
	if _, ok := harnessConfigString(t, chosen, "append_system_prompt"); ok {
		t.Fatal("text was appended to a prompt the caller chose whole")
	}
}

// The prompt is looked up in the directory the harness runs in, by the same
// rule the spawn uses: the session's own directory, else the instance's.
func TestInjectedContextUsesTheSpawnsWorkingDirectory(t *testing.T) {
	srv := &Server{cfg: &config.Config{}, agentStore: contextTestAgentStore(t), kanbanClient: fakeKanbanCards(t, map[string][]string{"card-billing": {"billing"}})}
	for name, run := range map[string]struct {
		sess *store.Session
		inst *msg.Instance
	}{
		"session directory":  {&store.Session{SessionID: "br_a", Harness: msg.HarnessCodex, CardID: "card-billing", WorkingDir: "/work/northwind/cmd"}, &msg.Instance{}},
		"instance directory": {&store.Session{SessionID: "br_b", Harness: msg.HarnessCodex, CardID: "card-billing"}, &msg.Instance{WorkingDir: "/work/northwind"}},
	} {
		srv.injectPromptContext(context.Background(), run.sess, run.inst)
		if prompt, _ := harnessConfigString(t, run.sess, "system_prompt"); !strings.Contains(prompt, "Northwind bills monthly.") {
			t.Errorf("%s: system_prompt = %q, want the northwind section", name, prompt)
		}
	}
}

// A session names a card kanban-store has, or is not created.
func TestCreateSessionChecksItsCard(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)
	srv.kanbanClient = fakeKanbanCards(t, map[string][]string{"card-billing": {"billing"}})
	create := func(cardID string) *http.Response {
		return doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
			Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
			Harness: msg.HarnessClaudeCode, InstanceID: instID, CardID: cardID,
		})
	}

	unknown := create("card-nobody-has")
	body, _ := io.ReadAll(unknown.Body)
	if unknown.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "unknown_card") {
		t.Fatalf("unknown card: status %d %s, want 400 unknown_card", unknown.StatusCode, body)
	}

	known := create("card-billing")
	if known.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(known.Body)
		t.Fatalf("known card: status %d %s", known.StatusCode, body)
	}
	created := decodeJSON[msg.ManagedSession](t, known)
	stored, err := st.GetSession(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CardID != "card-billing" {
		t.Fatalf("stored card_id = %q, want card-billing", stored.CardID)
	}
}
