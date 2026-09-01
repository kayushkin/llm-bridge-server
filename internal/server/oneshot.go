package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// handleInstanceOneShot runs a stateless single-turn LLM call against an
// instance's harness binary in -oneshot mode. The harness binary is expected
// to implement the -oneshot flag: read a msg.OneShotRequest JSON from stdin,
// emit a msg.OneShotResponse JSON on stdout, and exit. Binaries that don't
// implement -oneshot will exit non-zero and the error is surfaced verbatim.
//
// No session is created. No event is streamed. Use POST /sessions for those.
func (s *Server) handleInstanceOneShot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.harnessStore.GetInstance(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	if !inst.Enabled {
		http.Error(w, "instance disabled", http.StatusBadRequest)
		return
	}

	var req msg.OneShotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}

	raw, status, err := s.runOneShot(r.Context(), inst, req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// runOneShot executes one stateless schema-forced call on an instance's harness
// and returns the harness's own bytes.
//
// Split out of the HTTP handler so callers INSIDE this server can use it
// without making an HTTP request to themselves. The signal classifier is the
// first: it used to call api.anthropic.com with a key resolved from
// auth-store, on every turn end, which is the single largest remaining drain on
// that key. Going through the harness puts it on the subscription login
// instead, and no credential passes through this code at all.
//
// The harness's bytes are returned untouched, including on failure: a harness
// that wrote a structured error explains itself better than anything this layer
// could invent about it.
func (s *Server) runOneShot(ctx context.Context, inst *msg.Instance, req msg.OneShotRequest) ([]byte, int, error) {
	h := inst.HarnessType
	binPath, ok := harness.Available(h)
	if !ok {
		return nil, http.StatusBadGateway,
			fmt.Errorf("harness binary not found: %s", msg.HarnessBinaryName(h))
	}

	// Resolve which credential is bound to this instance. We pass the id (not
	// the secret) through env, mirroring how Start does it — harnesses that
	// need it call auth-store themselves; harnesses that don't (e.g. inber,
	// which inherits its credential from the running inber-server's env)
	// just ignore it.
	credID := resolveCredential(s.harnessStore, inst.ID)

	callCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()

	body, _ := json.Marshal(req)

	cmd := exec.CommandContext(callCtx, binPath, "-oneshot")
	cmd.Env = os.Environ()
	if credID != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_CREDENTIAL_ID="+credID)
	}
	cmd.Stdin = bytes.NewReader(body)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if stdout.Len() > 0 {
			// The harness explained itself. Pass it through verbatim.
			return stdout.Bytes(), http.StatusBadGateway, nil
		}
		return nil, http.StatusBadGateway,
			fmt.Errorf("exec %s -oneshot: %v (stderr: %s)", binPath, err, stderr.String())
	}
	return stdout.Bytes(), http.StatusOK, nil
}

// classifierOneShot runs the signal classifier's call on the configured harness
// instance.
//
// A missing or disabled instance is an error rather than a fallback to the API:
// falling back is how the drain this replaced went unnoticed for months, since
// a working classifier and a working-but-billing classifier look identical from
// outside.
func (s *Server) classifierOneShot(ctx context.Context, req msg.OneShotRequest) ([]byte, error) {
	id := s.cfg.SignalClassifierInstance
	if id == "" {
		return nil, fmt.Errorf("no signal-classifier instance configured")
	}
	inst, err := s.harnessStore.GetInstance(id)
	if err != nil {
		return nil, fmt.Errorf("signal-classifier instance %q: %w", id, err)
	}
	if !inst.Enabled {
		return nil, fmt.Errorf("signal-classifier instance %q is disabled", id)
	}
	raw, status, err := s.runOneShot(ctx, inst, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("signal-classifier oneshot returned %d: %s", status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
