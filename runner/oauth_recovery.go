package main

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/kayushkin/llm-bridge/msg"
)

var claudeAuthorizationURL = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?[^[:space:]]+`)

// ClaudeOAuthRecovery owns login subprocesses that must survive a failed
// Claude Code session. The bridge is the only caller: runners accept no
// command text, only the fixed subscription-login flow and its one-time code.
type ClaudeOAuthRecovery struct {
	mu        sync.Mutex
	processes map[string]io.WriteCloser
	outgoing  chan<- *msg.RunnerMessage
}

func NewClaudeOAuthRecovery(outgoing chan<- *msg.RunnerMessage) *ClaudeOAuthRecovery {
	return &ClaudeOAuthRecovery{processes: make(map[string]io.WriteCloser), outgoing: outgoing}
}

func (r *ClaudeOAuthRecovery) Start(recoveryID string) {
	if recoveryID == "" {
		r.Failed(recoveryID, fmt.Errorf("recovery id is required"))
		return
	}
	r.mu.Lock()
	if _, exists := r.processes[recoveryID]; exists {
		r.mu.Unlock()
		r.Failed(recoveryID, fmt.Errorf("recovery is already running"))
		return
	}
	r.mu.Unlock()

	command := exec.Command("claude", "auth", "login", "--claudeai")
	stdin, err := command.StdinPipe()
	if err != nil {
		r.Failed(recoveryID, err)
		return
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		r.Failed(recoveryID, err)
		return
	}
	if err := command.Start(); err != nil {
		r.Failed(recoveryID, err)
		return
	}
	r.mu.Lock()
	r.processes[recoveryID] = stdin
	r.mu.Unlock()

	go func() {
		defer func() { r.mu.Lock(); delete(r.processes, recoveryID); r.mu.Unlock() }()
		scanner := bufio.NewScanner(stdout)
		ready := false
		for scanner.Scan() {
			line := scanner.Text()
			if !ready {
				if url := claudeAuthorizationURL.FindString(line); url != "" {
					ready = true
					r.send(recoveryID, "ready", url, "")
				}
			}
		}
		if err := scanner.Err(); err != nil {
			r.Failed(recoveryID, err)
			return
		}
		if err := command.Wait(); err != nil {
			r.Failed(recoveryID, err)
			return
		}
		if !ready {
			r.Failed(recoveryID, fmt.Errorf("Claude login ended without an authorization URL"))
			return
		}
		r.send(recoveryID, "succeeded", "", "Claude login succeeded")
	}()
}

func (r *ClaudeOAuthRecovery) SubmitCode(recoveryID, code string) error {
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("authorization code is required")
	}
	r.mu.Lock()
	stdin := r.processes[recoveryID]
	r.mu.Unlock()
	if stdin == nil {
		return fmt.Errorf("no running recovery %s", recoveryID)
	}
	_, err := io.WriteString(stdin, code+"\n")
	return err
}

func (r *ClaudeOAuthRecovery) Failed(recoveryID string, err error) {
	r.send(recoveryID, "failed", "", err.Error())
}

func (r *ClaudeOAuthRecovery) send(recoveryID, action, url, message string) {
	r.outgoing <- &msg.RunnerMessage{Type: msg.RunnerMsgClaudeOAuthRecovery, ClaudeOAuthRecovery: &msg.RunnerClaudeOAuthRecovery{RecoveryID: recoveryID, Action: action, AuthorizationURL: url, Message: message}}
}
