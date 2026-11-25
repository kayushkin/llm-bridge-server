package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// runGateCommandWithStubCurl runs cmd in /bin/sh, as a harness runs a command
// hook, with a stub curl first on PATH that prints stubStdout and exits
// stubExit. It returns the shell's stdout, stderr and exit code.
func runGateCommandWithStubCurl(t *testing.T, cmd, stubStdout string, stubExit int) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte(stubStdout), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\ncat >/dev/null\ncat " + filepath.Join(dir, "stdout") + "\n"
	if stubExit != 0 {
		stub += "echo 'curl: stub failure' >&2\n"
	}
	stub += "exit " + strconv.Itoa(stubExit) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	shell := exec.Command("/bin/sh", "-c", cmd)
	shell.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
	shell.Stdin = strings.NewReader(`{"tool_name":"Bash"}`)
	var stdout, stderr bytes.Buffer
	shell.Stdout, shell.Stderr = &stdout, &stderr
	err := shell.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return stdout.String(), stderr.String(), code
}

// gateCommandsOfEveryHarness builds the permission gate of each harness that
// has one, so every test below holds all three to the same behavior.
func gateCommandsOfEveryHarness(t *testing.T) map[string]string {
	t.Helper()
	srv, _ := testServerWithHookStore(t)
	commands := map[string]string{}

	settings, err := srv.buildClaudeCodeSettings(&store.Session{SessionID: "s1", Harness: msg.HarnessClaudeCode})
	if err != nil {
		t.Fatalf("build claude code settings: %v", err)
	}
	pre := parseSettings(t, settings)["hooks"].(map[string]any)["PreToolUse"].([]any)
	inner := pre[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	commands["claude_code"], _ = inner["command"].(string)

	for harness, build := range map[string]func(*store.Session) (map[string][]any, error){
		"codex": srv.buildCodexHookConfig,
		"devin": func(sess *store.Session) (map[string][]any, error) {
			return srv.buildPrehookCommandHookTree(sess, msg.HarnessDevin)
		},
	} {
		tree, err := build(&store.Session{SessionID: "s1"})
		if err != nil {
			t.Fatalf("build %s hooks: %v", harness, err)
		}
		encoded, _ := json.Marshal(tree["PreToolUse"])
		var entries []struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(encoded, &entries); err != nil {
			t.Fatal(err)
		}
		commands[harness] = entries[0].Hooks[0].Command
	}
	for harness, command := range commands {
		if command == "" {
			t.Fatalf("%s: no permission gate command was built", harness)
		}
	}
	return commands
}

// The gate's answer must reach the harness exactly as curl printed it, with
// exit 0: that is what every tool call that reaches the gate depends on.
func TestPermissionGateCommandPassesTheGatesAnswerThroughUnchanged(t *testing.T) {
	answer := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"ok"}}`
	for harness, command := range gateCommandsOfEveryHarness(t) {
		stdout, stderr, code := runGateCommandWithStubCurl(t, command, answer, 0)
		if code != 0 || stdout != answer || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 0 and the gate's answer unchanged", harness, code, stdout, stderr)
		}
	}
}

// A post that fails must block the tool call. Exit 2 is the only hook exit
// that blocks; measured on Claude Code 2.1.330, the old http gate let a tool
// run when its port was dead or answered 503.
func TestPermissionGateCommandBlocksWhenThePostFails(t *testing.T) {
	for _, curlExit := range []int{7, 22, 28, 52, 56} { // refused, HTTP error, time limit, empty reply, dropped
		for harness, command := range gateCommandsOfEveryHarness(t) {
			_, stderr, code := runGateCommandWithStubCurl(t, command, "", curlExit)
			if code != 2 {
				t.Errorf("%s: curl exit %d gave hook exit %d, want 2 (the blocking exit)", harness, curlExit, code)
			}
			if !strings.Contains(stderr, permissionGateUnreachableMessage) || !strings.Contains(stderr, "curl: stub failure") {
				t.Errorf("%s: curl exit %d: stderr %q, want curl's own error and the blocking message", harness, curlExit, stderr)
			}
		}
	}
}

// curl must give up before the harness kills the hook, or the harness's own
// timeout, which lets the tool run, decides instead of the blocking exit.
func TestPermissionGateCurlGivesUpBeforeTheHookTimeout(t *testing.T) {
	if permissionGateCurlMaxTimeSeconds >= permissionGateHookTimeoutSeconds {
		t.Fatalf("curl max time %ds is not below the hook timeout %ds", permissionGateCurlMaxTimeSeconds, permissionGateHookTimeoutSeconds)
	}
	for harness, command := range gateCommandsOfEveryHarness(t) {
		if !strings.Contains(command, "--max-time 86100 ") {
			t.Errorf("%s: command %q does not limit curl to %ds", harness, command, permissionGateCurlMaxTimeSeconds)
		}
	}
}

// Codex trusts a hook by a hash of its command, stored under the hook's
// position, so a command that differs between sessions makes one session's
// trust undo another's. The codex and devin gates must be one string for every
// session, server address and harness, with nothing of the session in it.
func TestTheCodexAndDevinGateCommandIsTheSameInEverySession(t *testing.T) {
	want := permissionGateCommandPostingToTheURLInTheEnvironment()
	for _, listenAddr := range []string{"127.0.0.1:8160", ":18160", "https://bridge.example"} {
		srv, _ := testServerWithHookStore(t)
		srv.cfg.ListenAddr = listenAddr
		for _, harness := range []msg.Harness{msg.HarnessCodex, msg.HarnessDevin} {
			for _, sessionID := range []string{"br_1", "br_2", "a&touch x;#"} {
				tree, err := srv.buildPrehookCommandHookTree(&store.Session{SessionID: sessionID, Harness: harness}, harness)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(tree["PreToolUse"])
				var entries []struct {
					Hooks []struct {
						Command string `json:"command"`
					} `json:"hooks"`
				}
				if err := json.Unmarshal(encoded, &entries); err != nil {
					t.Fatal(err)
				}
				if got := entries[0].Hooks[0].Command; got != want {
					t.Errorf("%s at %s, session %q: gate command %q, want the one fixed command %q", harness, listenAddr, sessionID, got, want)
				}
			}
		}
	}
	if !strings.Contains(want, `"$`+permissionGateURLEnvironmentVariable+`"`) {
		t.Errorf("the gate command %q does not post to the quoted %s", want, permissionGateURLEnvironmentVariable)
	}
}

// The spawn hands codex and devin their gate's address in the variable the
// command reads, and hands it to no other harness.
func TestPermissionGateEnvironmentNamesTheSessionsGate(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	srv.cfg.ListenAddr = "127.0.0.1:8160"
	cases := map[msg.Harness]string{
		msg.HarnessCodex:      permissionGateURLEnvironmentVariable + "=http://127.0.0.1:8160/permission/codex-prehook/br_1",
		msg.HarnessDevin:      permissionGateURLEnvironmentVariable + "=http://127.0.0.1:8160/permission/devin-prehook/br_1",
		msg.HarnessClaudeCode: "",
		msg.HarnessMock:       "",
	}
	for harness, want := range cases {
		got := srv.permissionGateEnvironment(&store.Session{SessionID: "br_1", Harness: harness})
		if want == "" {
			if len(got) != 0 {
				t.Errorf("%s: environment %q, want none", harness, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: environment %q, want [%q]", harness, got, want)
		}
	}
	if got := srv.permissionGateEnvironment(&store.Session{Harness: msg.HarnessCodex}); len(got) != 0 {
		t.Errorf("a session with no id: environment %q, want none", got)
	}
}

// With the variable empty or unset the real curl has no address to post to,
// and the command must take the blocking exit, as it does for any failed post.
func TestTheEnvironmentGateBlocksWhenTheVariableIsEmptyOrUnset(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl on PATH")
	}
	command := permissionGateCommandPostingToTheURLInTheEnvironment()
	var withoutTheVariable []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, permissionGateURLEnvironmentVariable+"=") {
			withoutTheVariable = append(withoutTheVariable, variable)
		}
	}
	for name, environment := range map[string][]string{
		"unset": withoutTheVariable,
		"empty": append(append([]string{}, withoutTheVariable...), permissionGateURLEnvironmentVariable+"="),
	} {
		shell := exec.Command("/bin/sh", "-c", command)
		shell.Env = append(environment, "PATH="+filepath.Dir(curl)+":/usr/bin:/bin")
		shell.Stdin = strings.NewReader(`{"tool_name":"Bash"}`)
		var stdout, stderr bytes.Buffer
		shell.Stdout, shell.Stderr = &stdout, &stderr
		err := shell.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Errorf("%s: %v, stdout %q, stderr %q; want exit 2", name, err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), permissionGateUnreachableMessage) {
			t.Errorf("%s: stderr %q lacks the blocking message", name, stderr.String())
		}
	}
}
