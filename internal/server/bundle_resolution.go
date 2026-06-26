package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// injectBundleResolution auto-selects a session's tools from a curated bundle,
// based on the repository it runs in. It is OPT-IN: a session participates only
// when its HarnessConfig sets "auto_bundle": true. When active it:
//
//   - reads the repo path from HarnessConfig "work_dir"
//   - detects that repo's signature tags via repo-store /detect
//   - resolves a bundle for those tags (+ optional "task_tags") via
//     bundle-store /resolve
//   - merges the resolved tool-store tool names into "tool_store_tools" (a union
//     with any names the caller already supplied)
//
// It runs BEFORE injectMCPConfig in startOnInstance, so the resolved tools flow
// straight into the existing tool-store provisioning path (→ --mcp-config).
//
// Reachability failures (repo-store/bundle-store down, detect/resolve errors)
// are logged and skipped — a session must still start without a bundle. Only a
// genuinely unparseable HarnessConfig is returned as an error, matching
// injectMCPConfig. Resolved skills/model/effort are logged for now; surfacing
// skills into the session and applying model/effort defaults are follow-ups
// (skills install out-of-band; model/effort stay agent-store's domain).
func (s *Server) injectBundleResolution(sess *store.Session) error {
	if sess == nil || sess.Harness != msg.HarnessClaudeCode {
		return nil
	}
	if len(sess.HarnessConfig) == 0 {
		return nil
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		return fmt.Errorf("HarnessConfig unparseable: %w", err)
	}

	var autoBundle bool
	if raw, ok := cfg["auto_bundle"]; ok {
		_ = json.Unmarshal(raw, &autoBundle)
	}
	if !autoBundle {
		return nil
	}

	var workDir string
	if raw, ok := cfg["work_dir"]; ok {
		_ = json.Unmarshal(raw, &workDir)
	}
	if workDir == "" {
		log.Printf("[bundle] session %s: auto_bundle set but no work_dir; skipping", sess.SessionID)
		return nil
	}

	// An explicit mcp_config means the caller owns MCP wiring — don't fight it
	// (and injectMCPConfig would reject having both anyway).
	if _, exists := cfg["mcp_config"]; exists {
		log.Printf("[bundle] session %s: mcp_config already set; skipping bundle tool injection", sess.SessionID)
		return nil
	}

	var taskTags []string
	if raw, ok := cfg["task_tags"]; ok {
		_ = json.Unmarshal(raw, &taskTags)
	}

	tags, err := s.detectRepoTags(workDir)
	if err != nil {
		log.Printf("[bundle] session %s: repo-store detect %q failed: %v; skipping", sess.SessionID, workDir, err)
		return nil
	}
	resolved, err := s.resolveBundle(tags, taskTags)
	if err != nil {
		log.Printf("[bundle] session %s: bundle-store resolve failed: %v; skipping", sess.SessionID, err)
		return nil
	}

	var existing []string
	if raw, ok := cfg["tool_store_tools"]; ok {
		_ = json.Unmarshal(raw, &existing)
	}
	merged := unionStrings(existing, resolved.Tools)
	if len(merged) > 0 {
		b, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("marshal tool_store_tools: %w", err)
		}
		cfg["tool_store_tools"] = b
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal HarnessConfig: %w", err)
	}
	sess.HarnessConfig = out
	log.Printf("[bundle] session %s: repo=%s tags=%v → bundles=%v tools=%v skills=%v (model=%q effort=%q)",
		sess.SessionID, workDir, tags, resolved.Bundles, merged, resolved.Skills, resolved.Model, resolved.Effort)
	return nil
}

// detectRepoTags asks repo-store for a path's signature tags (stateless detect,
// so unregistered repos still resolve).
func (s *Server) detectRepoTags(path string) ([]string, error) {
	body, _ := json.Marshal(map[string]string{"path": path})
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := s.postJSON(s.cfg.RepoStoreURL+"/detect", body, &out); err != nil {
		return nil, err
	}
	return out.Tags, nil
}

type resolvedBundle struct {
	Bundles []string `json:"bundles"`
	Skills  []string `json:"skills"`
	Tools   []string `json:"tools"`
	Model   string   `json:"model"`
	Effort  string   `json:"effort"`
}

func (s *Server) resolveBundle(repoTags, taskTags []string) (*resolvedBundle, error) {
	body, _ := json.Marshal(map[string]any{"repo_tags": repoTags, "task_tags": taskTags})
	var out resolvedBundle
	if err := s.postJSON(s.cfg.BundleStoreURL+"/resolve", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Server) postJSON(url string, body []byte, out any) error {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, string(respBody))
	}
	return json.Unmarshal(respBody, out)
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, x := range a {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, x := range b {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
