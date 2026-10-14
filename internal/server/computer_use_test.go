package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func computerUseSession(t *testing.T, harnessName msg.Harness, config string) *store.Session {
	t.Helper()
	return &store.Session{SessionID: "br_computer_use", Harness: harnessName, HarnessConfig: json.RawMessage(config)}
}

func runnerInstance(allowed bool) *msg.Instance {
	return &msg.Instance{ID: "inst-ssdawn", Machine: &msg.Machine{
		ID: "m_ssdawn", Name: "SSDawn", Transport: msg.TransportRunner, ComputerUseAllowed: allowed,
	}}
}

func reportedTool(name string) *msg.ComputerUseTool {
	if name != "SSDawn" {
		return nil
	}
	return &msg.ComputerUseTool{Command: "/mnt/c/Users/slava/AppData/Local/llm-bridge/llm-bridge-computer-use.exe", Version: "test"}
}

func mcpServerNames(t *testing.T, sess *store.Session) map[string]json.RawMessage {
	t.Helper()
	cfg, err := harnessConfigMap(sess)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if err := json.Unmarshal(cfg["mcp_config"], &text); err != nil {
		t.Fatalf("mcp_config is not a string: %s", cfg["mcp_config"])
	}
	if !strings.HasPrefix(text, "{") {
		t.Fatalf("mcp_config is %q, want the config's JSON, not a path", text)
	}
	var config struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(text), &config); err != nil {
		t.Fatal(err)
	}
	return config.MCPServers
}

func TestComputerUseJoinsTheSessionsOtherServersUnderItsOwnName(t *testing.T) {
	srv := &Server{}
	sess := computerUseSession(t, msg.HarnessClaudeCode,
		`{"computer_use":true,"mcp_config":"{\"mcpServers\":{\"notes\":{\"command\":\"notes-mcp\"}}}"}`)
	if err := srv.injectComputerUse(sess, runnerInstance(true), reportedTool); err != nil {
		t.Fatal(err)
	}
	servers := mcpServerNames(t, sess)
	if _, ok := servers["notes"]; !ok {
		t.Error("the session's own server was dropped")
	}
	var computer struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(servers[msg.ComputerUseMCPServerName], &computer); err != nil || !strings.HasSuffix(computer.Command, "llm-bridge-computer-use.exe") {
		t.Errorf("computer-use server = %s (%v)", servers[msg.ComputerUseMCPServerName], err)
	}
	if _, reserved := servers["computer-use"]; reserved {
		t.Error("the server is named computer-use, which Claude Code drops")
	}
}

func TestComputerUseIsRefusedNamingWhatIsMissing(t *testing.T) {
	srv := &Server{}
	cases := []struct {
		name    string
		sess    *store.Session
		inst    *msg.Instance
		mention string
	}{
		{"switched off", computerUseSession(t, msg.HarnessClaudeCode, `{"computer_use":true}`), runnerInstance(false), "switched off"},
		{"not a runner", computerUseSession(t, msg.HarnessClaudeCode, `{"computer_use":true}`),
			&msg.Instance{ID: "inst-local", Machine: &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal, ComputerUseAllowed: true}}, "runner machine"},
		{"not claude code", computerUseSession(t, msg.HarnessCodex, `{"computer_use":true}`), runnerInstance(true), "Claude Code"},
		{"runner reported none", computerUseSession(t, msg.HarnessClaudeCode, `{"computer_use":true}`),
			&msg.Instance{ID: "inst-mac", Machine: &msg.Machine{ID: "m_mac", Name: "MacBook", Transport: msg.TransportRunner, ComputerUseAllowed: true}}, "no computer-use tool"},
		{"not a boolean", computerUseSession(t, msg.HarnessClaudeCode, `{"computer_use":"yes"}`), runnerInstance(true), "true or false"},
	}
	for _, c := range cases {
		err := srv.injectComputerUse(c.sess, c.inst, reportedTool)
		if err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%s: error = %v, want one mentioning %q", c.name, err, c.mention)
		}
	}
}

func TestASessionThatDidNotAskIsUntouched(t *testing.T) {
	srv := &Server{}
	sess := computerUseSession(t, msg.HarnessClaudeCode, `{"model":"x"}`)
	if err := srv.injectComputerUse(sess, runnerInstance(true), reportedTool); err != nil {
		t.Fatal(err)
	}
	if string(sess.HarnessConfig) != `{"model":"x"}` {
		t.Errorf("config changed to %s", sess.HarnessConfig)
	}
}

func TestARunnerSessionGetsItsMCPConfigFileAsJSON(t *testing.T) {
	srv := &Server{}
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"notes":{"command":"notes-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pathJSON, _ := json.Marshal(path)
	sess := computerUseSession(t, msg.HarnessClaudeCode, `{"mcp_config":`+string(pathJSON)+`}`)
	if err := srv.inlineMCPConfigForRunner(sess, runnerInstance(false)); err != nil {
		t.Fatal(err)
	}
	if _, ok := mcpServerNames(t, sess)["notes"]; !ok {
		t.Error("the file's server did not come through")
	}

	local := computerUseSession(t, msg.HarnessClaudeCode, `{"mcp_config":`+string(pathJSON)+`}`)
	if err := srv.inlineMCPConfigForRunner(local, &msg.Instance{ID: "inst-local", Machine: &msg.Machine{Transport: msg.TransportLocal}}); err != nil {
		t.Fatal(err)
	}
	if string(local.HarnessConfig) != `{"mcp_config":`+string(pathJSON)+`}` {
		t.Errorf("a local session's path was rewritten: %s", local.HarnessConfig)
	}
}
