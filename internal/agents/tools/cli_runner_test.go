package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubstituteBasic(t *testing.T) {
	out, err := substitute("hello {{name}}", map[string]any{"name": "world"})
	if err != nil || out != "hello world" {
		t.Fatalf("got %q err=%v", out, err)
	}
}

func TestSubstituteMultiple(t *testing.T) {
	out, err := substitute("{{a}}-{{b}}-{{a}}", map[string]any{"a": "X", "b": "Y"})
	if err != nil || out != "X-Y-X" {
		t.Fatalf("got %q err=%v", out, err)
	}
}

func TestSubstituteSpaces(t *testing.T) {
	out, err := substitute("{{ key }}", map[string]any{"key": "v"})
	if err != nil || out != "v" {
		t.Fatalf("got %q err=%v", out, err)
	}
}

func TestSubstituteMissing(t *testing.T) {
	_, err := substitute("{{missing}}", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestSubstituteNonString(t *testing.T) {
	out, err := substitute("count={{n}}", map[string]any{"n": float64(42)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != "count=42" {
		t.Fatalf("got %q", out)
	}
}

func TestSubstitutePassthrough(t *testing.T) {
	out, err := substitute("no placeholders", nil)
	if err != nil || out != "no placeholders" {
		t.Fatalf("got %q err=%v", out, err)
	}
}

func TestRunCLIEcho(t *testing.T) {
	tool := &Tool{
		Name: "echo",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "echo",
			ArgsTemplate: []string{"{{msg}}"},
		},
	}
	out, err := runCLI(context.Background(), tool, `{"msg":"hi"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out) != "hi" {
		t.Fatalf("output: %q", out)
	}
}

// TestRunCLIMissingField covers an ArgsTemplate placeholder with no matching
// input key. The error is the caller's only account of WHICH placeholder went
// unresolved, so this pins the tool name and the field name in it — asserting
// only that some error came back leaves runCLI free to return any of its other
// four.
func TestRunCLIMissingField(t *testing.T) {
	tool := &Tool{
		Name: "echo",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "echo",
			ArgsTemplate: []string{"{{nope}}"},
		},
	}
	_, err := runCLI(context.Background(), tool, `{}`)
	if err == nil {
		t.Fatal("expected error for missing field")
	}
	const want = `tool "echo": missing input field "nope" for placeholder {{nope}}`
	if err.Error() != want {
		t.Fatalf("substitution error:\n got %q\nwant %q", err.Error(), want)
	}
}

// TestRunCLINonZeroExit covers a command that fails with nothing on stderr.
// runCLI returns the exit error unwrapped in that arm, so the assertion is
// EQUALITY and not `strings.Contains`: every wrapping of the exit error still
// contains "exit status 1", and a contains-check would hold just as well if
// this arm started folding a prefix in front of it.
func TestRunCLINonZeroExit(t *testing.T) {
	tool := &Tool{
		Name: "false",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command: "false",
		},
	}
	_, err := runCLI(context.Background(), tool, `{}`)
	if err == nil {
		t.Fatal("expected error for nonzero exit")
	}
	if err.Error() != "exit status 1" {
		t.Fatalf("silent-stderr arm should return the exit error unwrapped, got %q", err.Error())
	}
}

// TestRunCLIMissingSpec covers the guard for a tool whose CLI spec is nil or
// has an empty command — runCLI must reject it rather than exec nothing. Both
// arms pin the message, which names the offending tool: that name is what tells
// an operator staring at a provisioning failure which of their tools is the
// misconfigured one.
func TestRunCLIMissingSpec(t *testing.T) {
	_, err := runCLI(context.Background(), &Tool{Name: "nospec", Kind: KindCLI}, `{}`)
	if err == nil {
		t.Fatal("expected error when CLI spec is nil")
	}
	if err.Error() != `cli spec missing for tool "nospec"` {
		t.Fatalf("nil-spec arm: got %q", err.Error())
	}
	_, err = runCLI(context.Background(), &Tool{Name: "nocmd", Kind: KindCLI, CLI: &CLISpec{}}, `{}`)
	if err == nil {
		t.Fatal("expected error when CLI command is empty")
	}
	if err.Error() != `cli spec missing for tool "nocmd"` {
		t.Fatalf("empty-command arm: got %q", err.Error())
	}
}

// TestRunCLIInvalidInputJSON covers rejection of malformed input JSON before
// any command runs. It pins the prefix AND that the decoder's own complaint is
// still wrapped inside it — the prefix alone says the input was bad, and the
// wrapped half is the only thing that says where.
func TestRunCLIInvalidInputJSON(t *testing.T) {
	tool := &Tool{Name: "echo", Kind: KindCLI, CLI: &CLISpec{Command: "echo"}}
	_, err := runCLI(context.Background(), tool, `{not json`)
	if err == nil {
		t.Fatal("expected error for invalid input json")
	}
	if !strings.HasPrefix(err.Error(), "invalid input json: ") {
		t.Fatalf("json error lost its prefix: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid character 'n'") {
		t.Fatalf("json error dropped the decoder's account of what was wrong: %q", err.Error())
	}
}

// TestRunCLITimeout exercises the per-tool timeout: a sleep that outlasts
// TimeoutMs must be canceled and surface an error rather than block.
func TestRunCLITimeout(t *testing.T) {
	tool := &Tool{
		Name: "slow",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "sleep",
			ArgsTemplate: []string{"5"},
			TimeoutMs:    50,
		},
	}
	start := time.Now()
	_, err := runCLI(context.Background(), tool, `{}`)
	if err == nil {
		t.Fatal("expected error from timed-out command")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout did not fire promptly: ran for %v", elapsed)
	}
}

// TestRunCLIContextCancel covers cancellation via the caller's context (as
// opposed to the per-tool timeout) — a canceled context must abort the run.
func TestRunCLIContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running
	tool := &Tool{
		Name: "slow",
		Kind: KindCLI,
		CLI:  &CLISpec{Command: "sleep", ArgsTemplate: []string{"5"}},
	}
	if _, err := runCLI(ctx, tool, `{}`); err == nil {
		t.Fatal("expected error from canceled context")
	}
}

// TestRunCLIStderrCapture verifies that on a non-zero exit the stderr text is
// folded into the returned error while stdout is still returned to the caller.
func TestRunCLIStderrCapture(t *testing.T) {
	tool := &Tool{
		Name: "noisy",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "sh",
			ArgsTemplate: []string{"-c", "echo out; echo boom 1>&2; exit 3"},
		},
	}
	out, err := runCLI(context.Background(), tool, `{}`)
	if err == nil {
		t.Fatal("expected error for nonzero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr not folded into error: %v", err)
	}
	if strings.TrimSpace(out) != "out" {
		t.Fatalf("stdout not captured on failure: %q", out)
	}
}

// TestRunCLIEnvInjection verifies that env-var names listed in Tool.EnvKeys are
// passed through to the child process (and only those that are actually set).
func TestRunCLIEnvInjection(t *testing.T) {
	const key, val = "TOOLSTORE_TEST_ENV", "secret-value"
	t.Setenv(key, val)
	tool := &Tool{
		Name:    "printenv",
		Kind:    KindCLI,
		EnvKeys: []string{key},
		CLI: &CLISpec{
			Command:      "sh",
			ArgsTemplate: []string{"-c", "printf %s \"$" + key + "\""},
		},
	}
	out, err := runCLI(context.Background(), tool, `{}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != val {
		t.Fatalf("env var not injected: got %q want %q", out, val)
	}
}

// TestRunCLIWorkingDir verifies that CLISpec.WorkingDir sets the child's cwd.
func TestRunCLIWorkingDir(t *testing.T) {
	dir := t.TempDir()
	tool := &Tool{
		Name: "pwd",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:    "pwd",
			WorkingDir: dir,
		},
	}
	out, err := runCLI(context.Background(), tool, `{}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Resolve symlinks on both sides (e.g. macOS /tmp -> /private/tmp) before
	// comparing the reported cwd to the requested working dir.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(out))
	if got != want {
		t.Fatalf("working dir not honored: got %q want %q", got, want)
	}
}

// TestRunCLIArgsNonStringValue confirms non-string input values are stringified
// into a single argv element (no shell splitting) when passed through a
// placeholder.
func TestRunCLIArgsNonStringValue(t *testing.T) {
	tool := &Tool{
		Name: "echo",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "echo",
			ArgsTemplate: []string{"{{n}}"},
		},
	}
	out, err := runCLI(context.Background(), tool, `{"n": 42}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("output: %q", out)
	}
}
