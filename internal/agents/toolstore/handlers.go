package toolstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// InvokeLocalFunc dispatches a kind=local tool by registry name with the
// JSON-encoded input body. Returns the tool's string output.
type InvokeLocalFunc func(ctx context.Context, name, input string) (string, error)

// ListLocalsFunc returns the in-process registered local tools — used for
// discovery on GET /locals.
type ListLocalsFunc func() []LocalDescriptor

// HandlerOptions wires optional callbacks into NewHandlers. Any nil field
// disables the corresponding endpoint(s) — the lib never assumes a specific
// runtime is in-process.
type HandlerOptions struct {
	InvokeLocal       InvokeLocalFunc       // POST /tools/by-name/{name}/invoke (kind=local)
	ListLocals        ListLocalsFunc        // GET  /locals
	ResolveCredential ResolveCredentialFunc // POST /provision (env-key resolution)
}

// Handlers answers the store's HTTP routes. The server that mounts the store
// registers each method under its own pattern, behind its own prefix and
// access rules; the paths in the method comments are relative to that prefix.
type Handlers struct {
	s    *Store
	opts HandlerOptions
}

// NewHandlers returns the route handlers of store, with the callbacks in
// options.
func NewHandlers(store *Store, options HandlerOptions) *Handlers {
	return &Handlers{s: store, opts: options}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Health answers GET /health.
func (h *Handlers) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ListKinds answers GET /kinds.
func (h *Handlers) ListKinds(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, Kinds)
}

// refuseHarnessTool answers 409 for a route tool-store cannot serve for a
// harness tool, because the harness runs it. Reports whether it answered.
func refuseHarnessTool(w http.ResponseWriter, t *Tool, route string) bool {
	if t.Kind != KindHarness {
		return false
	}
	writeErr(w, 409, fmt.Sprintf("tool %s is a built-in tool of the %s harness, which runs it as %s; tool-store has no %s for it", t.Name, t.Harness, t.HarnessToolName, route))
	return true
}

// ListTools answers GET /tools.
func (h *Handlers) ListTools(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{
		Kind:        Kind(q.Get("kind")),
		Harness:     q.Get("harness"),
		Tag:         q.Get("tag"),
		Query:       q.Get("q"),
		EnabledOnly: q.Get("enabled") == "true",
	}
	if f.Kind != "" && !f.Kind.Valid() {
		writeErr(w, 400, "invalid kind: "+string(f.Kind))
		return
	}
	if notSeenSince := q.Get("not_seen_since"); notSeenSince != "" {
		since, err := strconv.ParseInt(notSeenSince, 10, 64)
		if err != nil || since < 0 {
			writeErr(w, 400, "invalid not_seen_since: want unix seconds")
			return
		}
		f.NotSeenSince = &since
	}
	if lim := q.Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil {
			writeErr(w, 400, "invalid limit")
			return
		}
		f.Limit = n
	}
	tools, err := h.s.ListTools(r.Context(), f)
	if errors.Is(err, ErrNotSeenSinceNeedsHarnessKind) {
		writeErr(w, 400, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tools == nil {
		tools = []Tool{}
	}
	writeJSON(w, 200, tools)
}

// RecordObservedHarnessTools answers POST /harness-tools/observed.
func (h *Handlers) RecordObservedHarnessTools(w http.ResponseWriter, r *http.Request) {
	var request ObservedHarnessToolsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	response, err := h.s.RecordObservedHarnessTools(r.Context(), request)
	switch {
	case errors.Is(err, ErrInvalidObservedHarnessTools):
		writeErr(w, 400, err.Error())
	case errors.Is(err, ErrObservedNameHeldByAnotherKind):
		writeErr(w, 409, err.Error())
	case err != nil:
		writeErr(w, 500, err.Error())
	default:
		writeJSON(w, 200, response)
	}
}

// PatchTool answers PATCH /tools/{id}: it changes any of tags, description
// and enabled on one tool. An unknown field, a null value or an empty body is a 400, so a misspelled
// field is never taken for "change nothing".
func (h *Handlers) PatchTool(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	var patch ToolPatch
	for field, value := range fields {
		if string(value) == "null" {
			writeErr(w, 400, fmt.Sprintf("field %q is null; leave it out to keep it", field))
			return
		}
		var target any
		switch field {
		case "tags":
			patch.Tags = new([]string)
			target = patch.Tags
		case "description":
			patch.Description = new(string)
			target = patch.Description
		case "enabled":
			patch.Enabled = new(bool)
			target = patch.Enabled
		default:
			writeErr(w, 400, fmt.Sprintf("unknown field %q; PATCH takes tags, description, enabled", field))
			return
		}
		if err := json.Unmarshal(value, target); err != nil {
			writeErr(w, 400, fmt.Sprintf("field %q: %v", field, err))
			return
		}
	}
	tool, err := h.s.PatchTool(r.Context(), id, patch)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, 404, "tool not found")
	case errors.Is(err, ErrInvalidToolPatch):
		writeErr(w, 400, err.Error())
	case err != nil:
		writeErr(w, 500, err.Error())
	default:
		writeJSON(w, 200, tool)
	}
}

// UpsertTool answers POST /tools.
func (h *Handlers) UpsertTool(w http.ResponseWriter, r *http.Request) {
	var t Tool
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	inserted, err := h.s.UpsertTool(r.Context(), &t)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	status := 200
	if inserted {
		status = 201
	}
	writeJSON(w, status, t)
}

// GetTool answers GET /tools/{id}.
func (h *Handlers) GetTool(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	t, err := h.s.GetTool(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, t)
}

// GetToolByName answers GET /tools/by-name/{name}.
func (h *Handlers) GetToolByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, 400, "name required")
		return
	}
	t, err := h.s.GetToolByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, t)
}

// DeleteTool answers DELETE /tools/{id}.
func (h *Handlers) DeleteTool(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.DeleteTool(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// EnableTool answers POST /tools/{id}/enable.
func (h *Handlers) EnableTool(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, true)
}

// DisableTool answers POST /tools/{id}/disable.
func (h *Handlers) DisableTool(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, false)
}

func (h *Handlers) setEnabled(w http.ResponseWriter, r *http.Request, v bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.SetEnabled(r.Context(), id, v); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": v})
}

// InvokeByName answers POST /tools/by-name/{name}/invoke.
func (h *Handlers) InvokeByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, 400, "name required")
		return
	}
	t, err := h.s.GetToolByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if refuseHarnessTool(w, t, "invoke") {
		return
	}
	if !t.Enabled {
		writeErr(w, 409, "tool is disabled")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "read body: "+err.Error())
		return
	}
	switch t.Kind {
	case KindLocal:
		if h.opts.InvokeLocal == nil {
			writeErr(w, 501, "this server does not host local tool implementations")
			return
		}
		out, err := h.opts.InvokeLocal(r.Context(), name, string(body))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"output": out})
	case KindCLI:
		out, err := runCLI(r.Context(), t, string(body))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"output": out})
	case KindMCP:
		writeErr(w, 400, "mcp tools are not invokable via /invoke; use /provision to obtain a launcher config")
	default:
		writeErr(w, 500, "unknown kind: "+string(t.Kind))
	}
}

// SpecByName answers GET /tools/by-name/{name}/spec.
func (h *Handlers) SpecByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, 400, "name required")
		return
	}
	t, err := h.s.GetToolByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if refuseHarnessTool(w, t, "spec") {
		return
	}
	switch t.Kind {
	case KindCLI:
		writeJSON(w, 200, t.CLI)
	case KindMCP:
		writeJSON(w, 200, t.MCP)
	case KindLocal:
		writeErr(w, 400, "spec is only meaningful for kind=cli or kind=mcp; locals are invoked via /invoke")
	default:
		writeErr(w, 500, "unknown kind: "+string(t.Kind))
	}
}

// Provision answers POST /provision.
func (h *Handlers) Provision(w http.ResponseWriter, r *http.Request) {
	var req ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	resp, err := Provision(r.Context(), h.s, req, h.opts.ResolveCredential)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

// ListInstanceTools answers GET /instances/{id}/tools.
func (h *Handlers) ListInstanceTools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, 400, "instance id required")
		return
	}
	tools, err := h.s.ListInstanceTools(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tools == nil {
		tools = []Tool{}
	}
	writeJSON(w, 200, tools)
}

// EnableForInstance answers POST /instances/{id}/tools/by-name/{name}.
func (h *Handlers) EnableForInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	err := h.s.EnableForInstance(r.Context(), id, name)
	switch {
	case err == nil:
		writeJSON(w, 200, map[string]string{"instance_id": id, "tool": name, "status": "enabled"})
	case errors.Is(err, ErrNotFound):
		writeErr(w, 404, "tool not found")
	case errors.Is(err, ErrGloballyDisabled):
		writeErr(w, 409, err.Error())
	default:
		writeErr(w, 400, err.Error())
	}
}

// DisableForInstance answers DELETE /instances/{id}/tools/by-name/{name}.
func (h *Handlers) DisableForInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	err := h.s.DisableForInstance(r.Context(), id, name)
	switch {
	case err == nil:
		writeJSON(w, 200, map[string]string{"instance_id": id, "tool": name, "status": "disabled"})
	case errors.Is(err, ErrNotFound):
		writeErr(w, 404, "no opt-in for that (instance, tool)")
	default:
		writeErr(w, 400, err.Error())
	}
}

// ListInstancesForTool answers GET /tools/by-name/{name}/instances.
func (h *Handlers) ListInstancesForTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ids, err := h.s.ListInstancesForTool(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "tool not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, 200, ids)
}

// ListLocals answers GET /locals.
func (h *Handlers) ListLocals(w http.ResponseWriter, _ *http.Request) {
	if h.opts.ListLocals == nil {
		writeJSON(w, 200, []LocalDescriptor{})
		return
	}
	out := h.opts.ListLocals()
	if out == nil {
		out = []LocalDescriptor{}
	}
	writeJSON(w, 200, out)
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// ResolveCredentialFunc resolves a credential for an auth-store provider name
// to the key or token auth-store currently holds for it. Returns an error if
// the provider is unknown or no credential is enabled — provisioning fails
// loudly rather than producing a half-configured tool.
//
// It does not promise a value that still works, and the wording used to say
// "active", which read as if it did. auth-store refreshes an expired OAuth
// token only when the credential's refresh_mode is "server" and it is not
// leased; in every other case it answers 200 with the token it has stored and
// declares the risk in two response fields, expires_at and leased. A resolver
// that reads neither hands back a value auth-store already knows may be stale.
type ResolveCredentialFunc func(ctx context.Context, provider string) (string, error)
