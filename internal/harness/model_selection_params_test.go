package harness

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestBuildStartParamsCarriesTheTypedModel(t *testing.T) {
	sess := &store.Session{SessionID: "br_x", HarnessConfig: json.RawMessage(
		`{"model":"mock-model-alt","model_selection":{"model":"mock-model-alt","role":"efficient","selected_by":"session"}}`)}
	raw := buildStartParams(sess, "")
	var got StartParams
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "mock-model-alt" {
		t.Fatalf("typed Model = %q, want mock-model-alt", got.Model)
	}
	if got.ModelSelection == nil || got.ModelSelection.Role != msg.ModelRoleEfficient || got.ModelSelection.SelectedBy != msg.ModelSelectedBySession {
		t.Fatalf("typed ModelSelection = %+v, want role=efficient selected_by=session", got.ModelSelection)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["model"]) != `"mock-model-alt"` {
		t.Fatalf("wire key model = %s; every bridge reads this key and it must carry the id", wire["model"])
	}
}

func TestBuildStartParamsReportsAbsenceAsAbsence(t *testing.T) {
	raw := buildStartParams(&store.Session{SessionID: "br_y"}, "")
	var got StartParams
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "" || got.ModelSelection != nil {
		t.Fatalf("no config → Model=%q Selection=%+v; want empty and nil, never a guess", got.Model, got.ModelSelection)
	}
	var wire map[string]json.RawMessage
	json.Unmarshal(raw, &wire)
	if _, present := wire["model"]; present {
		t.Fatal("a session with no model must not put a model key on the wire")
	}
}
