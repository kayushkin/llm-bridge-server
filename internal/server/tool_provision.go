package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// injectMCPConfig opts a session into tool-store-driven MCP provisioning.
//
// If the session's HarnessConfig contains a "tool_store_tools" key (a JSON
// array of tool names), this calls tool-store's /provision endpoint with
// those names, writes the returned MCP server config JSON to a tmpfile, and
// rewrites HarnessConfig so that:
//   - "mcp_config" holds the tmpfile path (consumed by claudecode via
//     --mcp-config; opaque pass-through for other harnesses)
//   - "tool_store_tools" is removed (it's an instruction, not state to ship)
//
// If the key is absent, this is a no-op — provisioning is opt-in. If present
// but provisioning fails, the error is logged and the session start aborts
// upstream (caller decides; we return an error instead of swallowing).
//
// The mutation is in-memory only, matching injectHookSettings.
func (s *Server) injectMCPConfig(sess *store.Session) error {
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
	raw, ok := cfg["tool_store_tools"]
	if !ok {
		return nil
	}
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("tool_store_tools is not a string array: %w", err)
	}
	if len(tools) == 0 {
		delete(cfg, "tool_store_tools")
		merged, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("re-marshal HarnessConfig: %w", err)
		}
		sess.HarnessConfig = merged
		return nil
	}

	// If the caller already set mcp_config, refuse — two sources of truth for
	// the same field. Surface loudly per "fail fast and loud".
	if _, exists := cfg["mcp_config"]; exists {
		return fmt.Errorf("HarnessConfig has both mcp_config and tool_store_tools; pick one")
	}

	body, _ := json.Marshal(map[string]any{"tools": tools})
	url := s.cfg.ToolStoreURL + "/provision"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build provision request: %w", err)
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
		return fmt.Errorf("read tool-store response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tool-store /provision returned %d: %s", resp.StatusCode, string(respBody))
	}

	dir := filepath.Join(os.TempDir(), "llm-bridge-mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return fmt.Errorf("create mcp tmpfile: %w", err)
	}
	if _, err := f.Write(respBody); err != nil {
		f.Close()
		os.Remove(f.Name())
		return fmt.Errorf("write mcp tmpfile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("close mcp tmpfile: %w", err)
	}

	pathJSON, _ := json.Marshal(f.Name())
	cfg["mcp_config"] = pathJSON
	delete(cfg, "tool_store_tools")

	merged, err := json.Marshal(cfg)
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("re-marshal HarnessConfig: %w", err)
	}
	sess.HarnessConfig = merged
	log.Printf("[tool-store] provisioned %d tools for session %s → %s", len(tools), sess.BridgeID, f.Name())
	return nil
}
