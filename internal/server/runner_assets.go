package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// runnerAssetsDir is the on-disk directory the bridge server uses for
// runner binary distribution and the installer script. Configurable via
// LLMBRIDGE_RUNNER_ASSETS_DIR; defaults to /usr/local/lib/llm-bridge-runner-binaries.
//
// Layout:
//
//	<dir>/llm-bridge-runner-<os>-<arch>   prebuilt binaries
//	<dir>/install.sh                       optional override for the installer
//
// If install.sh is absent the server falls back to serving the script
// from the runner repo's known path (~/repos/llm-bridge-runner/scripts/install.sh).
// Both paths are read at request time so dropping a new binary in place
// is enough — no server restart required.
func runnerAssetsDir() string {
	if d := os.Getenv("LLMBRIDGE_RUNNER_ASSETS_DIR"); d != "" {
		return d
	}
	return "/usr/local/lib/llm-bridge-runner-binaries"
}

// handleRunnerBinary serves a prebuilt llm-bridge-* binary matching the
// requested os+arch. Query params:
//
//	os=linux|darwin
//	arch=amd64|arm64
//	name=runner|claudecode|… (optional, defaults to "runner")
//
// On disk the file is expected to be llm-bridge-<name>-<os>-<arch>.
// Used by both the install script (default name=runner) and the runner
// itself when it needs to fetch a missing harness wrapper binary.
func (s *Server) handleRunnerBinary(w http.ResponseWriter, r *http.Request) {
	osQ := r.URL.Query().Get("os")
	archQ := r.URL.Query().Get("arch")
	nameQ := r.URL.Query().Get("name")
	if nameQ == "" {
		nameQ = "runner"
	}
	if osQ == "" || archQ == "" {
		http.Error(w, "os and arch query params are required", http.StatusBadRequest)
		return
	}
	if !looksSafe(osQ) || !looksSafe(archQ) || !looksSafe(nameQ) {
		http.Error(w, "invalid os/arch/name", http.StatusBadRequest)
		return
	}

	name := fmt.Sprintf("llm-bridge-%s-%s-%s", nameQ, osQ, archQ)
	path := filepath.Join(runnerAssetsDir(), name)
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("binary not available for %s/%s", osQ, archQ), http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, stat.ModTime(), f)
}

// handleRunnerInstallScript serves the install.sh script. Looked up in
// (in order) LLMBRIDGE_RUNNER_INSTALL_SCRIPT env var, the assets dir's
// install.sh, then the canonical repo path.
func (s *Server) handleRunnerInstallScript(w http.ResponseWriter, r *http.Request) {
	candidates := []string{
		os.Getenv("LLMBRIDGE_RUNNER_INSTALL_SCRIPT"),
		filepath.Join(runnerAssetsDir(), "install.sh"),
		filepath.Join(os.Getenv("HOME"), "repos", "llm-bridge-runner", "scripts", "install.sh"),
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			continue
		}
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		http.ServeContent(w, r, "install.sh", stat.ModTime(), f)
		return
	}
	http.Error(w, "install.sh not found", http.StatusNotFound)
}

// looksSafe is a paranoid filter for query-param values that get
// concatenated into filesystem paths. Allows only [a-z0-9_-].
func looksSafe(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return !strings.HasPrefix(s, "-")
}
