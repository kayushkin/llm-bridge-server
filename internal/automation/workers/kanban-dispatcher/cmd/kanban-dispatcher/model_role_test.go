package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/sessionmodelrole"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// stubModelRoles stands in for model-store and the bridge's harness table.
// Every harness runs the default role; the openai-only role runs on codex
// alone.
type stubModelRoles struct{}

func (stubModelRoles) Check(role string, harness msg.Harness) (sessionmodelrole.Model, error) {
	switch role {
	case "default":
		return sessionmodelrole.Model{ID: "model-test", Provider: "test"}, nil
	case "openai-only":
		if harness != msg.HarnessCodex {
			return sessionmodelrole.Model{}, fmt.Errorf("harness %s runs only anthropic models", harness)
		}
		return sessionmodelrole.Model{ID: "gpt-test", Provider: "openai"}, nil
	}
	return sessionmodelrole.Model{}, fmt.Errorf("model-store has no role %q", role)
}

// A fresh session carries the role as its own model choice, so the bridge's
// per-harness default never decides what it runs on.
func TestAFreshSessionNamesTheModelRole(t *testing.T) {
	in, err := buildBridgeInbound("tcl-sess", "x", "do it", true, cardSessionSpec{Harness: msg.HarnessClaudeCode, InstanceID: "inst-cc-local"}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if string(in.Create.HarnessConfig) != `{"model":"default"}` {
		t.Errorf("harness_config = %s, want the role", in.Create.HarnessConfig)
	}
	if _, err := buildBridgeInbound("tcl-sess", "x", "do it", true, cardSessionSpec{}, ""); err == nil {
		t.Error("a fresh session was built with no role")
	}
}

// A card whose harness cannot run the role's first model is not dispatched:
// nothing is published, linked or moved, and the tick names the cause.
func TestACardWhoseHarnessCannotRunTheRoleIsNotDispatched(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
		columnBacklog: {testCard("card-a", "group:a")},
	})
	kanban.effective["card-a"] = kanbanclient.EffectiveDefaults{Defaults: map[kanbanclient.DefaultField]kanbanclient.EffectiveDefault{kanbanclient.DefaultFieldInstance: fromBoard("inst-cc-local")}}
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()

	var published []messages.BridgeInbound
	err := run(runOptions{ModelRole: "openai-only", ModelRoles: stubModelRoles{},
		BoardID: testBoardID, KanbanURL: kanbanServer.URL, BridgeURL: bridgeServer.URL, MaxDispatch: 10,
		Publish: func(in messages.BridgeInbound) error {
			published = append(published, in)
			return nil
		},
	})
	wantShortfall(t, err, "card-a", "kanban_dispatcher_model_role", "runs only anthropic")
	if len(published) != 0 || len(kanban.linked) != 0 || len(kanban.moved) != 0 {
		t.Errorf("published %d, linked %v, moved %v for a card that must not start", len(published), kanban.linked, kanban.moved)
	}
}

func TestATickWithNoModelRoleStartsNothing(t *testing.T) {
	err := run(runOptions{BoardID: testBoardID, KanbanURL: "http://127.0.0.1:1", MaxDispatch: 10})
	if err == nil || !strings.Contains(err.Error(), "no model role") {
		t.Errorf("err = %v, want a refusal before any read", err)
	}
}
