package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Computer use on runner machines.
//
// A runner that installed a computer-use MCP server names its command in its
// Hello (msg.RunnerHello.ComputerUse). A session asks for it with
// harness_config "computer_use": true, and gets it only when all of these
// hold, or the spawn is refused saying which one failed:
//
//   - the harness is Claude Code, the one harness that takes an MCP config;
//   - the instance is on a runner machine (this host has no screen);
//   - the machine's owner switched it on (machines.computer_use_allowed,
//     PUT /machines/{id}/computer-use), off by default because the tool drives
//     that machine's screen, mouse and keyboard;
//   - the machine's runner is connected now and reported the tool.
//
// The tool joins whatever MCP servers the session already has, under
// msg.ComputerUseMCPServerName, and "computer_use" stays in the config so the
// runner is told to make the tool current before it starts the harness.

const harnessConfigComputerUseKey = "computer_use"

// sessionAsksForComputerUse reads harness_config "computer_use". Anything but
// a JSON boolean is an error, so a typo cannot quietly mean "no".
func sessionAsksForComputerUse(cfg map[string]json.RawMessage) (bool, error) {
	raw, present := cfg[harnessConfigComputerUseKey]
	if !present {
		return false, nil
	}
	var asks bool
	if err := json.Unmarshal(raw, &asks); err != nil {
		return false, fmt.Errorf("harness_config.%s must be true or false, got %s", harnessConfigComputerUseKey, raw)
	}
	return asks, nil
}

func harnessConfigMap(sess *store.Session) (map[string]json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(sess.HarnessConfig) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		return nil, fmt.Errorf("parse HarnessConfig: %w", err)
	}
	return cfg, nil
}

// mcpServersOfConfig returns the servers of a session's mcp_config, which is
// a path to a config file on this host or the config's JSON itself (Claude
// Code's --mcp-config takes either). Absent, it is an empty set.
func mcpServersOfConfig(cfg map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, present := cfg["mcp_config"]
	if !present {
		return map[string]json.RawMessage{}, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("harness_config.mcp_config must be a string (a path or the config's JSON), got %s", raw)
	}
	text := []byte(value)
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		content, err := os.ReadFile(value)
		if err != nil {
			return nil, fmt.Errorf("read mcp_config %s: %w", value, err)
		}
		text = content
	}
	var config struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(text, &config); err != nil {
		return nil, fmt.Errorf("mcp_config is not a Claude Code MCP config: %w", err)
	}
	if config.MCPServers == nil {
		config.MCPServers = map[string]json.RawMessage{}
	}
	return config.MCPServers, nil
}

func setInlineMCPConfig(cfg map[string]json.RawMessage, servers map[string]json.RawMessage) error {
	text, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return err
	}
	value, err := json.Marshal(string(text))
	if err != nil {
		return err
	}
	cfg["mcp_config"] = value
	return nil
}

// inlineMCPConfigForRunner replaces a session's mcp_config path with the
// file's JSON when the session runs on a runner machine: the path names a
// file on this host, and Claude Code there stopped with "MCP config file not
// found" (br_1792016058819150718, 2026-10-14). A config that is already JSON
// is left as it is.
func (s *Server) inlineMCPConfigForRunner(sess *store.Session, inst *msg.Instance) error {
	if inst.Machine == nil || inst.Machine.Transport != msg.TransportRunner {
		return nil
	}
	cfg, err := harnessConfigMap(sess)
	if err != nil {
		return err
	}
	raw, present := cfg["mcp_config"]
	if !present {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil && strings.HasPrefix(strings.TrimSpace(value), "{") {
		return nil
	}
	servers, err := mcpServersOfConfig(cfg)
	if err != nil {
		return err
	}
	if err := setInlineMCPConfig(cfg, servers); err != nil {
		return err
	}
	return s.replaceHarnessConfig(sess, cfg)
}

// injectComputerUse adds the runner's computer-use server to a session that
// asked for it, or refuses the spawn naming the condition that failed.
// runnerComputerUse is the tool a machine's connected runner reported
// (harness.Manager.RunnerComputerUse in the spawn).
func (s *Server) injectComputerUse(sess *store.Session, inst *msg.Instance, runnerComputerUse func(machineName string) *msg.ComputerUseTool) error {
	cfg, err := harnessConfigMap(sess)
	if err != nil {
		return err
	}
	asks, err := sessionAsksForComputerUse(cfg)
	if err != nil || !asks {
		return err
	}
	machine := inst.Machine
	switch {
	case sess.Harness != msg.HarnessClaudeCode:
		return fmt.Errorf("computer use is offered to Claude Code sessions only; this one runs %s", sess.Harness)
	case machine == nil || machine.Transport != msg.TransportRunner:
		return fmt.Errorf("computer use needs a runner machine with a screen; instance %s is not on one", inst.ID)
	case !machine.ComputerUseAllowed:
		return fmt.Errorf("computer use is switched off on machine %s (%s); its owner can switch it on with PUT /machines/%s/computer-use", machine.Name, machine.ID, machine.ID)
	}
	tool := runnerComputerUse(machine.Name)
	if tool == nil {
		return fmt.Errorf("the runner on %s has no computer-use tool to offer (not connected, or it could not install one; its log says why)", machine.Name)
	}
	servers, err := mcpServersOfConfig(cfg)
	if err != nil {
		return err
	}
	server, err := json.Marshal(map[string]any{"command": tool.Command, "args": []string{}})
	if err != nil {
		return err
	}
	servers[msg.ComputerUseMCPServerName] = server
	if err := setInlineMCPConfig(cfg, servers); err != nil {
		return err
	}
	log.Printf("[computer-use] session %s on %s gets %s (%s, version %s)", sess.SessionID, machine.Name, msg.ComputerUseMCPServerName, tool.Command, tool.Version)
	return s.replaceHarnessConfig(sess, cfg)
}

// handleSetMachineComputerUse is PUT /machines/{id}/computer-use with
// {"allowed": true|false}: the owner's switch, set alone so no other machine
// edit can flip it.
func (s *Server) handleSetMachineComputerUse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Allowed *bool `json:"allowed"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.Allowed == nil {
		http.Error(w, `body must be {"allowed": true} or {"allowed": false}`, http.StatusBadRequest)
		return
	}
	if err := s.harnessStore.SetMachineComputerUseAllowed(id, *req.Allowed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "machine not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	machine, err := s.harnessStore.GetMachine(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[computer-use] machine %s (%s): computer use allowed=%v", machine.Name, machine.ID, machine.ComputerUseAllowed)
	writeJSON(w, machine)
}
