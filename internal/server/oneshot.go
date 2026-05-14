package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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

	h := inst.HarnessType
	binPath, ok := harness.Available(h)
	if !ok {
		http.Error(w, fmt.Sprintf("harness binary not found: %s", msg.HarnessBinaryName(h)), http.StatusBadGateway)
		return
	}

	// Resolve which credential is bound to this instance. We pass the id (not
	// the secret) through env, mirroring how Start does it — harnesses that
	// need it call auth-store themselves; harnesses that don't (e.g. inber,
	// which inherits its credential from the running inber-server's env)
	// just ignore it.
	credID := resolveCredential(s.harnessStore, inst.ID)

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()

	body, _ := json.Marshal(req)

	cmd := exec.CommandContext(ctx, binPath, "-oneshot")
	cmd.Env = os.Environ()
	if credID != "" {
		cmd.Env = append(cmd.Env, "LLMBRIDGE_CREDENTIAL_ID="+credID)
	}
	cmd.Stdin = bytes.NewReader(body)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// If the harness wrote a structured error JSON to stdout, surface it
		// verbatim (transparent layer). Otherwise wrap exec error + stderr.
		if stdout.Len() > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.Copy(w, &stdout)
			return
		}
		http.Error(w, fmt.Sprintf("exec %s -oneshot: %v (stderr: %s)", binPath, err, stderr.String()), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, &stdout)
}
