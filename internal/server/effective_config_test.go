package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func settingByKey(t *testing.T, cfg *msg.EffectiveConfig, key msg.EffectiveSettingKey) msg.EffectiveSetting {
	t.Helper()
	for _, s := range cfg.Settings {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no %s setting in %+v", key, cfg.Settings)
	return msg.EffectiveSetting{}
}

// The view reads the same tool decision the spawn makes: a bundle narrowed by
// the principal's grants is attributed to the grants, with the bundle's full
// list in the notes.
func TestEffectiveConfigReportsBundleToolsNarrowedByGrants(t *testing.T) {
	bundles, _ := fakeBundleStore(t, "6", []int64{13, 14}, nil)
	grants := fakeGrantStore(t, "principal_000001", []string{"14", "99"})
	tools, asked := fakeToolStoreWithOptIns(t, nil)
	s := newServerWithBundles(tools.URL, bundles.URL, grants.URL)
	// Pinned at create, as every real row is; this bare server has no
	// model-store to resolve one live.
	sess := &store.Session{
		Harness: msg.HarnessClaudeCode, SessionID: "br_test", InstanceID: "inst-1", BundleID: "6", PrincipalID: "principal_000001",
		HarnessConfig: json.RawMessage(`{"model":"claude-opus-5","model_selection":{"model":"claude-opus-5","selected_by":"session"}}`),
	}

	cfg := s.effectiveConfigFor(context.Background(), sess, msg.EffectiveConfigSubject{SessionID: "br_test", Harness: msg.HarnessClaudeCode}, nil)

	toolsSetting := settingByKey(t, cfg, msg.EffectiveSettingTools)
	if toolsSetting.Layer != msg.EffectiveLayerGrant {
		t.Fatalf("tools layer = %s, want grant (the grants narrowed the bundle)", toolsSetting.Layer)
	}
	rows, _ := toolsSetting.Value.([]map[string]any)
	if len(rows) != 1 || rows[0]["id"] != int64(14) {
		t.Fatalf("tools value = %v, want the one tool both the bundle and the grants name (14)", toolsSetting.Value)
	}
	if !strings.Contains(strings.Join(toolsSetting.Notes, " "), "[13 14]") {
		t.Fatalf("notes should name the bundle's full list: %v", toolsSetting.Notes)
	}
	if len(*asked) != 0 {
		t.Fatalf("the view provisioned %d time(s); it must provision nothing", len(*asked))
	}
	bundle := settingByKey(t, cfg, msg.EffectiveSettingBundle)
	if bundle.Layer != msg.EffectiveLayerRequest || bundle.Value != "6" {
		t.Fatalf("bundle = %+v, want the requested bundle 6", bundle)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", cfg.Warnings)
	}
}

// A model pinned at create is attributed to the layer that chose it then,
// and the view says it was pinned.
func TestEffectiveConfigAttributesAPinnedModel(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	sess := &store.Session{
		Harness: msg.HarnessClaudeCode, SessionID: "br_test",
		HarnessConfig: json.RawMessage(`{"model":"claude-opus-5","model_selection":{"model":"claude-opus-5","selected_by":"prefs"},"effort":"high","permission_mode":"auto"}`),
	}
	cfg := s.effectiveConfigFor(context.Background(), sess, msg.EffectiveConfigSubject{SessionID: "br_test", Harness: msg.HarnessClaudeCode}, nil)

	model := settingByKey(t, cfg, msg.EffectiveSettingModel)
	if model.Layer != msg.EffectiveLayerHarnessDefault || model.Value != "claude-opus-5" || model.Record != "bridge-prefs.defaults.claude_code.model" {
		t.Fatalf("model = %+v, want claude-opus-5 from the harness default", model)
	}
	if !strings.Contains(strings.Join(model.Notes, " "), "pinned") {
		t.Fatalf("the view should say the model was pinned at create: %v", model.Notes)
	}
	effort := settingByKey(t, cfg, msg.EffectiveSettingEffort)
	if effort.Layer != msg.EffectiveLayerSession || effort.Value != "high" {
		t.Fatalf("effort = %+v, want high from the session", effort)
	}
	mode := settingByKey(t, cfg, msg.EffectiveSettingPermissionMode)
	if mode.Layer != msg.EffectiveLayerSession || mode.Value != "auto" {
		t.Fatalf("permission mode = %+v, want auto from the session", mode)
	}
	tools := settingByKey(t, cfg, msg.EffectiveSettingTools)
	if tools.Layer != msg.EffectiveLayerNone {
		t.Fatalf("with no tool-store configured tools = %+v, want none", tools)
	}
}

// A dry run for a board takes the board's defaults where the caller named
// none, and attributes each to the board or to the tag rule that set it.
func TestDryRunEffectiveConfigTakesTheBoardsDefaults(t *testing.T) {
	srv, _, instanceID := testServerWithInstance(t, msg.HarnessClaudeCode)
	kanban := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/boards/board-1/effective-defaults" || r.URL.Query().Get("tag") != "urgent" {
			t.Errorf("unexpected kanban-store request %s", r.URL.String())
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"board_id":"board-1","tags":["urgent"],"matched_rule_ids":["tr_3"],"defaults":{
			"default_instance_id":{"value":"` + instanceID + `","source":{"kind":"board"}},
			"default_principal_id":{"value":"principal_000007","source":{"kind":"tag_rule","rule_id":"tr_3","rule_tags":["urgent"]}},
			"default_bundle_id":{"value":"6","source":{"kind":"board"}}}}`))
	}))
	t.Cleanup(kanban.Close)
	srv.kanbanClient = kanbanclient.New(kanban.URL)

	req := httptest.NewRequest("GET", "/effective-config?harness=claude_code&board_id=board-1&tag=urgent", nil)
	w := httptest.NewRecorder()
	srv.handleDryRunEffectiveConfig(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var cfg msg.EffectiveConfig
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.Subject.DryRun || cfg.Subject.BoardID != "board-1" {
		t.Fatalf("subject = %+v", cfg.Subject)
	}
	principal := settingByKey(t, &cfg, msg.EffectiveSettingPrincipal)
	if principal.Layer != msg.EffectiveLayerTagRule || principal.Value != "principal_000007" || !strings.Contains(principal.Record, "tr_3") {
		t.Fatalf("principal = %+v, want principal_000007 from tag rule tr_3", principal)
	}
	instance := settingByKey(t, &cfg, msg.EffectiveSettingInstance)
	if instance.Layer != msg.EffectiveLayerBoard || instance.Value != instanceID {
		t.Fatalf("instance = %+v, want %s from the board", instance, instanceID)
	}
	bundle := settingByKey(t, &cfg, msg.EffectiveSettingBundle)
	if bundle.Layer != msg.EffectiveLayerBoard || bundle.Value != "6" {
		t.Fatalf("bundle = %+v, want 6 from the board", bundle)
	}
	// No bundle-store and no grant-store on this test server: the view must
	// say the spawn would be refused rather than go quiet.
	joined := strings.Join(cfg.Warnings, " | ")
	if !strings.Contains(joined, "bundle-store") || !strings.Contains(joined, "grant-store") {
		t.Fatalf("warnings should name the missing bundle-store and grant-store: %v", cfg.Warnings)
	}
	if len(cfg.Layers) != len(msg.EffectiveConfigLayers) {
		t.Fatalf("the legend must be served with the answer")
	}
}
