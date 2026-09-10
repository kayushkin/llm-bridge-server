package server

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

// dispatchTables makes the mock harness dispatchable: it accepts provider
// "mock" and implements -oneshot (cmd/mock-harness echoes the model it got).
func dispatchTables(srv *Server) {
	srv.harnessProviders = map[msg.Harness][]string{msg.HarnessMock: {"mock"}}
	srv.oneShotCapable = map[msg.Harness]bool{msg.HarnessMock: true}
}

func TestInstanceForModelPicksAnEnabledInstanceWhoseHarnessSupportsTheProvider(t *testing.T) {
	srv, _, instID := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	m, _, err := srv.resolveModelRow("efficient")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := srv.instanceForModel(m, true)
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != instID {
		t.Fatalf("chose %s, want %s", inst.ID, instID)
	}
}

func TestInstanceForModelRefusesWhenNoHarnessSupportsTheProvider(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	if err := srv.modelStore.AddProvider(modelstore.Provider{ID: "elsewhere", Name: "Elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.modelStore.AddModel(modelstore.Model{ID: "other-model", Provider: "elsewhere", Name: "Other", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m, _, _ := srv.resolveModelRow("other-model")
	_, err := srv.instanceForModel(m, true)
	if err == nil || !strings.Contains(err.Error(), `provider "elsewhere"`) {
		t.Fatalf("err = %v; want a refusal naming the unsupported provider", err)
	}
}

func TestInstanceForModelRefusesWhenTheSupportingHarnessLacksOneShot(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	srv.oneShotCapable = map[msg.Harness]bool{} // mock supports the provider but has no -oneshot
	m, _, _ := srv.resolveModelRow("efficient")
	_, err := srv.instanceForModel(m, true)
	if err == nil || !strings.Contains(err.Error(), "-oneshot") {
		t.Fatalf("err = %v; want a refusal that names the missing -oneshot mode", err)
	}
	// A session (not a one-shot) on the same model is fine.
	if _, err := srv.instanceForModel(m, false); err != nil {
		t.Fatalf("session dispatch should not need -oneshot: %v", err)
	}
}

func TestInstanceForModelIgnoresHarnessesSilentAboutProviders(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	srv.harnessProviders = map[msg.Harness][]string{msg.HarnessMock: nil} // silent, not "anything"
	m, _, _ := srv.resolveModelRow("efficient")
	if _, err := srv.instanceForModel(m, true); err == nil {
		t.Fatal("a harness with no explicit provider list was dispatched to; want a refusal")
	}
}

func TestOneShotRouteResolvesTheRoleAndDispatches(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, _, instID := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	resp := doJSON(t, srv, "POST", "/oneshot", msg.OneShotRequest{Prompt: "hi", Model: "efficient"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-LLM-Bridge-Model"); got != "mock-model-alt" {
		t.Fatalf("X-LLM-Bridge-Model=%q, want mock-model-alt (the efficient role's id)", got)
	}
	if got := resp.Header.Get("X-LLM-Bridge-Instance"); got != instID {
		t.Fatalf("X-LLM-Bridge-Instance=%q, want %s", got, instID)
	}
	if got := resp.Header.Get("X-LLM-Bridge-Model-Role"); got != "efficient" {
		t.Fatalf("X-LLM-Bridge-Model-Role=%q, want efficient", got)
	}
	var out msg.OneShotResponse
	if err := json.Unmarshal(body, &out); err != nil || out.Model != "mock-model-alt" {
		t.Fatalf("harness reported model %q (err %v); want the resolved id to have reached it", out.Model, err)
	}
}

func TestOneShotRouteRequiresAModelAndRefusesUnknownOnes(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	resp := doJSON(t, srv, "POST", "/oneshot", msg.OneShotRequest{Prompt: "hi"})
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("no model: status=%d, want 400", resp.StatusCode)
	}
	resp = doJSON(t, srv, "POST", "/oneshot", msg.OneShotRequest{Prompt: "hi", Model: "claude-imaginary"})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "model_unresolvable") {
		t.Fatalf("unknown model: status=%d body=%s", resp.StatusCode, b)
	}
}

func TestOneShotRouteRefusesWhenNoInstanceCanRunTheModel(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	srv.oneShotCapable = map[msg.Harness]bool{}
	resp := doJSON(t, srv, "POST", "/oneshot", msg.OneShotRequest{Prompt: "hi", Model: "efficient"})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(b), "no_capable_instance") {
		t.Fatalf("status=%d body=%s; want 503 no_capable_instance — never a silent pick", resp.StatusCode, b)
	}
}

func TestInstanceOneShotResolvesRolesAndChecksTheProvider(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, _, instID := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	resp := doJSON(t, srv, "POST", "/instances/"+instID+"/oneshot", msg.OneShotRequest{Prompt: "hi", Model: "efficient"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-LLM-Bridge-Model") != "mock-model-alt" {
		t.Fatalf("status=%d model=%q body=%s", resp.StatusCode, resp.Header.Get("X-LLM-Bridge-Model"), body)
	}
	// A model on a provider this instance's harness does not support is refused.
	if err := srv.modelStore.AddProvider(modelstore.Provider{ID: "elsewhere", Name: "Elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.modelStore.AddModel(modelstore.Model{ID: "other-model", Provider: "elsewhere", Name: "Other", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	resp = doJSON(t, srv, "POST", "/instances/"+instID+"/oneshot", msg.OneShotRequest{Prompt: "hi", Model: "other-model"})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(body), "model_not_runnable_on_instance") {
		t.Fatalf("status=%d body=%s; want 400 model_not_runnable_on_instance", resp.StatusCode, body)
	}
}

func TestInstanceOneShotWithNoModelUsesTheRegistryDefaultNotAnAccountDefault(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, _, instID := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	resp := doJSON(t, srv, "POST", "/instances/"+instID+"/oneshot", msg.OneShotRequest{Prompt: "hi"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-LLM-Bridge-Model") != "mock-model" || resp.Header.Get("X-LLM-Bridge-Model-Role") != "default" {
		t.Fatalf("status=%d model=%q role=%q body=%s; want the registry default role", resp.StatusCode, resp.Header.Get("X-LLM-Bridge-Model"), resp.Header.Get("X-LLM-Bridge-Model-Role"), body)
	}
}

func TestClassifierOneShotDispatchesByProviderWhenNoInstanceIsPinned(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, _, _ := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	srv.cfg.SignalClassifierInstance = "" // no pin: the model's provider decides
	raw, err := srv.classifierOneShot(context.Background(), msg.OneShotRequest{Prompt: "classify", Model: "efficient"})
	if err != nil {
		t.Fatal(err)
	}
	var out msg.OneShotResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.Model != "mock-model-alt" {
		t.Fatalf("classifier ran on %q (err %v); want the efficient role's mock-model-alt", out.Model, err)
	}
}

func TestClassifierOneShotRefusesAPinnedInstanceThatCannotRunTheModel(t *testing.T) {
	srv, _, instID := testServerWithInstance(t, msg.HarnessMock)
	dispatchTables(srv)
	srv.harnessProviders = map[msg.Harness][]string{msg.HarnessMock: {"not-mock"}}
	srv.cfg.SignalClassifierInstance = instID
	_, err := srv.classifierOneShot(context.Background(), msg.OneShotRequest{Prompt: "classify", Model: "efficient"})
	if err == nil {
		t.Fatal("a pinned instance whose harness cannot run the model was used; want a refusal")
	}
}
