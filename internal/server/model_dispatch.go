package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

// Dispatch by model: given a model the registry knows, find an instance whose
// harness can actually run it.
//
// Roles are global — "efficient" names a capability tier, not a provider — and
// the point of the bridge is that a one-shot call can run on ANY harness. That
// only holds if the server, not the caller, picks the instance from the
// resolved model's provider. Before this, every caller pinned an instance by
// hand (the classifier by config, the workers by flag, the renamer by copying
// the target's), and a role whose model lived on another provider had nowhere
// to go.
//
// Two measured tables decide eligibility. harnessSupportedProviders (health.go)
// says which providers each harness accepts; only an EXPLICIT entry counts here
// — a harness that is silent about providers is not dispatched to, because
// sending a model to whichever harness happened to say nothing is the guess this
// exists to end. harnessOneShotCapable says which harnesses implement the
// `-oneshot` mode runOneShot execs; a harness without it fails the exec, so it
// is excluded up front with a message that says so.

// harnessOneShotCapable lists the harness bridges that implement `-oneshot`.
//
// Measured 2026-09-10 by grepping each ~/repos/llm-bridge-*/main.go for
// "oneshot": only llm-bridge-claudecode has it; codex, hermes and the mock have
// none. Re-measure the same way when a bridge gains the mode and move it here —
// do not edit this to make a dispatch succeed. The consequence today is
// concrete: a role pointing at an OpenAI model has no oneshot-capable instance,
// and dispatch refuses rather than picking a harness that cannot run it.
var harnessOneShotCapable = map[msg.Harness]bool{
	msg.HarnessClaudeCode: true,
}

// resolveModelRow resolves what a caller asked for — a registry id, an alias,
// or a canonical role name — to the registry row, plus the role if one was
// named. This is the single resolution step every dispatch path shares; the
// row carries the provider that dispatch needs.
func (s *Server) resolveModelRow(requested string) (*modelstore.Model, msg.ModelRole, error) {
	if s.modelStore == nil {
		return nil, "", fmt.Errorf("resolve %q: model-store is not configured, and the registry is the only source of a model", requested)
	}
	if requested == "" {
		return nil, "", fmt.Errorf("resolve model: nothing requested")
	}
	if modelstore.IsCanonicalRole(requested) {
		m, err := s.modelStore.ResolveRole(requested)
		if err != nil {
			return nil, "", fmt.Errorf("role %q: %w", requested, err)
		}
		return m, msg.ModelRole(requested), nil
	}
	m, err := s.modelStore.ResolveModel(requested)
	if err != nil {
		return nil, "", fmt.Errorf("%q is not a model, alias or role the registry knows: %w", requested, err)
	}
	return m, "", nil
}

// harnessSupportsProvider reports whether the harness EXPLICITLY lists the
// provider. A missing or nil entry is "unknown", not "anything".
func (s *Server) harnessSupportsProvider(h msg.Harness, provider string) bool {
	for _, p := range s.harnessProviders[h] {
		if p == provider {
			return true
		}
	}
	return false
}

// instanceForModel picks an enabled instance whose harness explicitly supports
// the model's provider — and, when needOneShot is set, implements `-oneshot`.
// Instances on a local-transport machine are preferred; ties break on id so the
// choice is deterministic. When nothing qualifies the error says which of the
// three conditions failed, so the fix (assign a role differently, enable an
// instance, or give a bridge a oneshot mode) is named rather than guessed.
func (s *Server) instanceForModel(m *modelstore.Model, needOneShot bool) (*msg.Instance, error) {
	if m == nil {
		return nil, fmt.Errorf("dispatch: nil model")
	}
	if s.harnessStore == nil {
		return nil, fmt.Errorf("dispatch %s: harness-store is not configured", m.ID)
	}
	var supporting []msg.Harness
	for h := range s.harnessProviders {
		if s.harnessSupportsProvider(h, m.Provider) {
			supporting = append(supporting, h)
		}
	}
	sort.Slice(supporting, func(i, j int) bool { return supporting[i] < supporting[j] })
	if len(supporting) == 0 {
		return nil, fmt.Errorf("dispatch %s: no harness explicitly supports provider %q (harnessSupportedProviders)", m.ID, m.Provider)
	}
	var capable []msg.Harness
	for _, h := range supporting {
		if !needOneShot || s.oneShotCapable[h] {
			capable = append(capable, h)
		}
	}
	if len(capable) == 0 {
		return nil, fmt.Errorf("dispatch %s: harness(es) %v support provider %q but none implements -oneshot (harnessOneShotCapable); a one-shot on this model has nowhere to run", m.ID, supporting, m.Provider)
	}

	instances, err := s.harnessStore.ListInstances()
	if err != nil {
		return nil, fmt.Errorf("dispatch %s: list instances: %w", m.ID, err)
	}
	type candidate struct {
		inst  msg.Instance
		local bool
	}
	var candidates []candidate
	var skipped []string
	for _, inst := range instances {
		if !inst.Enabled {
			continue
		}
		eligible := false
		for _, h := range capable {
			if inst.HarnessType == h {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
		machine, err := s.harnessStore.GetMachine(inst.MachineID)
		if err != nil {
			// An instance whose machine cannot be loaded cannot spawn either
			// (startOnInstance fails the same lookup). Excluded, and named
			// below if it turns out to be the only one.
			skipped = append(skipped, fmt.Sprintf("%s (machine %q: %v)", inst.ID, inst.MachineID, err))
			continue
		}
		candidates = append(candidates, candidate{inst: inst, local: machine.Transport == msg.TransportLocal})
	}
	if len(candidates) == 0 {
		detail := ""
		if len(skipped) > 0 {
			detail = "; excluded: " + strings.Join(skipped, ", ")
		}
		return nil, fmt.Errorf("dispatch %s: no enabled instance of harness(es) %v to run provider %q%s", m.ID, capable, m.Provider, detail)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].local != candidates[j].local {
			return candidates[i].local
		}
		return candidates[i].inst.ID < candidates[j].inst.ID
	})
	chosen := candidates[0].inst
	return &chosen, nil
}

// setModelDispatchHeaders records on the response which model ran where and
// which layer chose it — metadata beside the harness's own bytes, which pass
// through untouched.
func setModelDispatchHeaders(w http.ResponseWriter, m *modelstore.Model, role msg.ModelRole, selectedBy msg.ModelSelectedBy, inst *msg.Instance) {
	w.Header().Set("X-LLM-Bridge-Model", m.ID)
	if role != "" {
		w.Header().Set("X-LLM-Bridge-Model-Role", string(role))
	}
	w.Header().Set("X-LLM-Bridge-Model-Selected-By", string(selectedBy))
	if inst != nil {
		w.Header().Set("X-LLM-Bridge-Instance", inst.ID)
	}
}

// writeErrorCode writes the {"error":{"code","message"}} shape this API uses.
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	http.Error(w, string(body), status)
}

// handleOneShot is the instance-less one-shot: the caller names a model (a
// registry id, alias, or role) and the server picks the instance from the
// model's provider. The body is msg.OneShotRequest; Model is required here,
// because with no instance named there is nothing else to derive one from.
func (s *Server) handleOneShot(w http.ResponseWriter, r *http.Request) {
	var req msg.OneShotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	if req.Prompt == "" {
		writeErrorCode(w, http.StatusBadRequest, "prompt_required", "prompt required")
		return
	}
	if req.Model == "" {
		writeErrorCode(w, http.StatusBadRequest, "model_required", "model required: a registry id, an alias, or a role ("+strings.Join(modelstore.CanonicalRoles, "/")+")")
		return
	}
	m, role, err := s.resolveModelRow(req.Model)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "model_unresolvable", err.Error())
		return
	}
	inst, err := s.instanceForModel(m, true)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "no_capable_instance", err.Error())
		return
	}
	requested := req.Model
	req.Model = m.ID // the harness receives the id, never the role or alias
	log.Printf("[oneshot] %q -> %s (role %q) on %s (%s)", requested, m.ID, string(role), inst.ID, inst.HarnessType)
	raw, status, err := s.runOneShot(r.Context(), inst, req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	setModelDispatchHeaders(w, m, role, msg.ModelSelectedBySession, inst)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
