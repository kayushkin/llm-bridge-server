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

// injectMCPConfig gives a session the MCP servers it is entitled to, from
// either of two sources, and rewrites HarnessConfig so "mcp_config" holds the
// path of the generated config file (consumed by claudecode via --mcp-config;
// opaque pass-through for other harnesses).
//
// The two sources, in precedence order:
//
//  1. A "tool_store_tools" key on HarnessConfig — a JSON array of tool names
//     the caller asked for by hand. The key is removed afterwards: it is an
//     instruction, not state to ship. An empty array means "no MCP servers",
//     and is honoured as an explicit opt-out.
//  2. Otherwise, the opt-in list the session's instance carries in tool-store —
//     the rows the Tools page writes. This is what makes a tick on that page
//     reach a spawned session instead of sitting in a table nothing reads.
//
// The two sources fail differently, on purpose. A named list that cannot be
// provisioned aborts the spawn: the caller asked for those tools, and starting
// without them starts something other than what was asked for. An instance's
// opt-ins are a standing preference rather than a per-session request, so a
// tool-store that is down or a stale opt-in row logs loudly and the session
// starts with no MCP servers — the same state every session on this box is in
// today. A registry outage must not stop the fleet from working.
//
// If the caller already set "mcp_config" it wins outright and neither source is
// consulted.
//
// The mutation is in-memory only, matching injectHookSettings.
func (s *Server) injectMCPConfig(sess *store.Session) error {
	if sess == nil || sess.Harness != msg.HarnessClaudeCode {
		return nil
	}

	cfg := map[string]json.RawMessage{}
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			return fmt.Errorf("HarnessConfig unparseable: %w", err)
		}
	}

	raw, named := cfg["tool_store_tools"]
	_, preset := cfg["mcp_config"]

	// Two sources of truth for the same field. Surface loudly per "fail fast
	// and loud".
	if named && preset {
		return fmt.Errorf("HarnessConfig has both mcp_config and tool_store_tools; pick one")
	}
	if preset {
		return nil
	}

	if named {
		var tools []string
		if err := json.Unmarshal(raw, &tools); err != nil {
			return fmt.Errorf("tool_store_tools is not a string array: %w", err)
		}
		delete(cfg, "tool_store_tools")
		if len(tools) == 0 {
			return s.replaceHarnessConfig(sess, cfg)
		}
		path, err := s.writeProvisionedMCPConfig(map[string]any{"tools": tools})
		if err != nil {
			return err
		}
		if path == "" {
			return fmt.Errorf("tool-store provisioned no servers for tools %v", tools)
		}
		return s.setMCPConfigPath(sess, cfg, path, len(tools))
	}

	// Instance defaults. Nothing to look up for a session with no instance.
	if sess.InstanceID == "" {
		return nil
	}
	path, err := s.writeProvisionedMCPConfig(map[string]any{"instance_id": sess.InstanceID})
	if err != nil {
		log.Printf("[tool-store] instance %s opt-ins unavailable for session %s, starting with no MCP servers: %v",
			sess.InstanceID, sess.SessionID, err)
		return nil
	}
	if path == "" {
		// The common case: nobody has ticked a tool for this instance.
		return nil
	}
	return s.setMCPConfigPath(sess, cfg, path, 0)
}

// setMCPConfigPath points HarnessConfig's mcp_config at path and writes the
// result back onto the session. toolCount is only for the log line; 0 means
// the tools came from the instance's opt-in list rather than a named request.
func (s *Server) setMCPConfigPath(sess *store.Session, cfg map[string]json.RawMessage, path string, toolCount int) error {
	pathJSON, err := json.Marshal(path)
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("marshal mcp config path: %w", err)
	}
	cfg["mcp_config"] = pathJSON
	if err := s.replaceHarnessConfig(sess, cfg); err != nil {
		os.Remove(path)
		return err
	}
	if toolCount > 0 {
		log.Printf("[tool-store] provisioned %d named tools for session %s → %s", toolCount, sess.SessionID, path)
	} else {
		log.Printf("[tool-store] provisioned instance %s opt-ins for session %s → %s", sess.InstanceID, sess.SessionID, path)
	}
	return nil
}

func (s *Server) replaceHarnessConfig(sess *store.Session, cfg map[string]json.RawMessage) error {
	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal HarnessConfig: %w", err)
	}
	sess.HarnessConfig = merged
	return nil
}

// writeProvisionedMCPConfig posts body to tool-store's /provision and writes
// the returned MCP server config to a tmpfile, returning its path. An empty
// returned path means tool-store had no servers to hand back — no file is
// written, because an --mcp-config pointing at an empty server set is a way to
// make a harness fail on nothing.
func (s *Server) writeProvisionedMCPConfig(body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal provision request: %w", err)
	}
	url := s.cfg.ToolStoreURL + "/provision"
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build provision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read tool-store response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tool-store /provision returned %d: %s", resp.StatusCode, string(respBody))
	}

	var provisioned struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(respBody, &provisioned); err != nil {
		return "", fmt.Errorf("tool-store /provision returned unparseable JSON: %w", err)
	}
	if len(provisioned.MCPServers) == 0 {
		return "", nil
	}

	dir := filepath.Join(os.TempDir(), "llm-bridge-mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create mcp tmpfile: %w", err)
	}
	if _, err := f.Write(respBody); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write mcp tmpfile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close mcp tmpfile: %w", err)
	}
	return f.Name(), nil
}
