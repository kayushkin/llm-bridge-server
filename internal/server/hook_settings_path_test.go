package server

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A session id reaches this file straight off the `POST /sessions` body:
// server/sessions.go mints one only when the caller sent none, and otherwise
// takes req.SessionID verbatim, checking it for collision and for nothing
// else. Card f02351f2 measures that endpoint as unauthenticated on *:8160 and
// answering from the public IP, so every probe below is a string a caller can
// actually choose.
//
// The id lands in two different KINDS of place and they do not have the same
// safe form:
//
//	claude-code  a JSON "url" value, consumed by CC's native http hook
//	codex        a shell command line, run by codex as a type:"command" hook
//
// So there are two properties pinned by two instruments. The URL property is
// checked by parsing. The shell property is checked by handing the generated
// command to a real /bin/sh with a stub curl on PATH and looking at what the
// shell did with it -- a string search for metacharacters answers a question
// about the string, and the shell answers the question about the command.

type pathProbe struct{ name, id string }

// pathProbes are ids chosen so that each one separates a different wrong
// answer. MARKER is replaced by the caller with a path inside its temp dir.
func pathProbes() []pathProbe {
	return []pathProbe{
		{"well formed", "br_1712345678901234567"},
		{"slash", "a/b"},
		{"space", "a b"},
		{"question mark", "a?x=1"},
		{"hash", "a#frag"},
		{"single quote", "a'b"},
		{"semicolon", "a;touch MARKER"},
		// The one url.PathEscape does not cover. '&' is a reserved sub-delim
		// that is legal inside a path segment, so PathEscape passes it
		// through unchanged -- and in a shell command line it separates two
		// commands. This probe is the reason the codex site needs more than
		// the escaping the card prescribes.
		{"ampersand", "a&touch MARKER"},
		// Also passed straight through by PathEscape. Inert in a URL, a
		// variable expansion in an unquoted shell word.
		{"dollar", "a$SENTINEL"},
		// ⚠️ The marker probes above are a LOWER BOUND on their own, and this
		// one is why. Whether an injected command leaves its marker behind
		// depends on what follows the id ON THE LINE: at the hook sites the
		// URL is last, so `a&touch MARKER` runs touch with no arguments and
		// the file appears; in the renamer prompt the curl flags trail the
		// URL, so touch is handed `-H` and exits with "invalid option"
		// leaving nothing. Measured both ways. A missing marker is NOT
		// evidence the injection failed. Terminating the payload with `;#`
		// comments out the remainder, so this probe's marker appears wherever
		// the id reaches a shell at all.
		{"comment-terminated", "a&touch MARKER;#"},
	}
}

// assertSegments parses rawURL and requires that its path is exactly the
// given template, with the id occupying exactly ONE segment. The empty string
// marks the id's slot -- it is not always the last one (the renamer's URL is
// /sessions/<id>/auto-rename), and an assertion that assumed "last" passed on
// a URL whose id had moved.
//
// It reads EscapedPath rather than Path deliberately: Path is the decoded
// form, so a %2F that must stay inside one segment has already turned back
// into a separator by the time you read it, and the assertion passes on
// unrepaired code.
func assertSegments(t *testing.T, label, rawURL, id string, template ...string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Errorf("%s: url.Parse(%q): %v", label, rawURL, err)
		return
	}
	if u.RawQuery != "" || u.Fragment != "" {
		t.Errorf("%s: the id escaped its path segment into the query or fragment: %q (query=%q fragment=%q)",
			label, rawURL, u.RawQuery, u.Fragment)
		return
	}
	segments := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(segments) != len(template) {
		t.Errorf("%s: want %d path segments, got %d: %q", label, len(template), len(segments), u.EscapedPath())
		return
	}
	for i, want := range template {
		if want != "" {
			if segments[i] != want {
				t.Errorf("%s: path segment %d = %q, want %q (full: %q)", label, i, segments[i], want, u.EscapedPath())
				return
			}
			continue
		}
		got, err := url.PathUnescape(segments[i])
		if err != nil {
			t.Errorf("%s: segment %d = %q does not decode: %v", label, i, segments[i], err)
			return
		}
		if got != id {
			t.Errorf("%s: path segment %d decodes to %q, want the id %q (full: %q)", label, i, got, id, u.EscapedPath())
			return
		}
	}
}

func TestCCPermissionHookURLKeepsTheSessionIDInOneSegment(t *testing.T) {
	for _, probe := range pathProbes() {
		t.Run(probe.name, func(t *testing.T) {
			id := strings.ReplaceAll(probe.id, "MARKER", filepath.Join(t.TempDir(), "INJECTED"))
			srv, _ := testServerWithHookStore(t)
			got, err := srv.buildClaudeCodeSettings(&store.Session{SessionID: id, Harness: msg.HarnessClaudeCode})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			parsed := parseSettings(t, got)
			pre := parsed["hooks"].(map[string]any)["PreToolUse"].([]any)
			inner := pre[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			raw, _ := inner["url"].(string)
			assertSegments(t, probe.name, raw, id, "permission", "cc-prehook", "")
		})
	}
}

// runGeneratedCommand hands cmd to /bin/sh exactly as a harness would, with a
// stub `curl` first on PATH that appends its argv to a log and exits 0. It
// returns the recorded invocations, one []string per run.
func runGeneratedCommand(t *testing.T, cmd string) [][]string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	stub := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >>" + logPath + "; done\n" +
		"printf -- '--\\n' >>" + logPath + "\n" +
		"cat >/dev/null 2>&1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write curl stub: %v", err)
	}

	sh := exec.Command("/bin/sh", "-c", cmd)
	// The stub curl is FIRST on PATH, but the real /usr/bin and /bin stay on
	// it. That is deliberate: with a stripped PATH the injected `touch` fails
	// with "not found" and the marker check below can never fire, so the
	// control reads green because the harness disarmed the payload rather
	// than because the code did.
	sh.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "SENTINEL=EXPANDED")
	sh.Stdin = strings.NewReader("{}")
	if out, err := sh.CombinedOutput(); err != nil {
		// A non-zero exit is information, not a failure: "the shell refused
		// it" is a legitimate outcome for some repairs, and the caller
		// decides whether it is the outcome it wanted.
		t.Logf("sh exited non-zero for %q: %v (output: %s)", cmd, err, out)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var runs [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if line == "--" {
			runs = append(runs, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return runs
}

// assertOneCurlRun is the shell half of the pin, shared by the codex probes
// and the two user-hook tests.
func assertOneCurlRun(t *testing.T, label, cmd, marker, id string, template ...string) {
	t.Helper()
	runs := runGeneratedCommand(t, cmd)

	// 1. The shell ran curl exactly once. Two runs means a separator inside
	//    the id started a second command; zero means the shell never reached
	//    curl at all.
	if len(runs) != 1 {
		t.Fatalf("%s: the shell ran curl %d times, want exactly 1 (command: %q)", label, len(runs), cmd)
	}

	// 2. Nothing else ran. The semicolon and ampersand probes ask the shell
	//    to touch a marker; if it exists, the session id executed.
	if marker != "" {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the session id EXECUTED -- %q exists after running %q", label, marker, cmd)
		}
	}

	// 3. Exactly one argv entry is a URL, it is one word, and it addresses
	//    the intended endpoint with the id in exactly one path segment.
	//    Requiring exactly one also catches a repair that leaves the URL
	//    split across two words: the tail word is then not a URL and the
	//    head word's path is short.
	argv := runs[0]
	var urls []string
	for _, a := range argv {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			urls = append(urls, a)
		}
	}
	if len(urls) != 1 {
		t.Fatalf("%s: %d argv entries look like a URL, want exactly 1: %q", label, len(urls), argv)
	}
	assertSegments(t, label, urls[0], id, template...)
}

func TestCodexPermissionHookCommandRunsExactlyOneCurl(t *testing.T) {
	for _, probe := range pathProbes() {
		t.Run(probe.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "INJECTED")
			id := strings.ReplaceAll(probe.id, "MARKER", marker)
			if !strings.Contains(probe.id, "MARKER") {
				marker = ""
			}

			srv, _ := testServerWithHookStore(t)
			byEvent, err := srv.buildCodexHookConfig(&store.Session{SessionID: id, Harness: msg.HarnessCodex})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			cmd := codexPermissionCommand(t, byEvent)
			assertOneCurlRun(t, probe.name, cmd, marker, id, "permission", "codex-prehook", "")
		})
	}
}

// codexPermissionCommand digs the permission gate's command string out of the
// builder's return value.
func codexPermissionCommand(t *testing.T, byEvent map[string][]any) string {
	t.Helper()
	encoded, err := json.Marshal(byEvent["PreToolUse"])
	if err != nil {
		t.Fatalf("marshal PreToolUse: %v", err)
	}
	var entries []struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(encoded, &entries); err != nil {
		t.Fatalf("unmarshal PreToolUse: %v", err)
	}
	for _, e := range entries {
		for _, h := range e.Hooks {
			if strings.Contains(h.Command, "/permission/codex-prehook/") {
				return h.Command
			}
		}
	}
	t.Fatalf("no codex permission hook generated: %s", encoded)
	return ""
}

// The two user-hook sites carry hook-store's id rather than the caller's.
// ids.NewHookID mints "hook_" + a Crockford-base32 ULID and handleCreateHook
// overwrites whatever the caller sent, so there is nothing in that alphabet to
// escape -- see the scope-fence arm in the scorer, which un-escapes it and
// records that the pin stays green. What these two DO pin is the shell shape,
// which is a property of the command builder and not of the id.

func TestCCUserHookCommandRunsExactlyOneCurl(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	seedHook(t, hks, "hook_01J0GLOBAL", "PreToolUse", "*", msg.HookScopeGlobal, "")
	got, err := srv.buildClaudeCodeSettings(&store.Session{SessionID: "b1", Harness: msg.HarnessClaudeCode})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	parsed := parseSettings(t, got)
	pre := parsed["hooks"].(map[string]any)["PreToolUse"].([]any)

	var found bool
	for _, e := range pre {
		inner := e.(map[string]any)["hooks"].([]any)[0].(map[string]any)
		cmd, _ := inner["command"].(string)
		if !strings.Contains(cmd, "/hooks/exec/") {
			continue
		}
		found = true
		assertOneCurlRun(t, "cc user hook", cmd, "", "hook_01J0GLOBAL", "hooks", "exec", "")
	}
	if !found {
		t.Fatalf("no /hooks/exec/ command generated: %v", pre)
	}
}

func TestCodexUserHookCommandRunsExactlyOneCurl(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	h := &msg.Hook{
		ID: "hook_01J0CODEX", Harness: msg.HarnessCodex, Event: "PreToolUse",
		Matcher: "*", Command: ":", ScopeKind: msg.HookScopeGlobal, Enabled: true,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatalf("create codex hook: %v", err)
	}
	byEvent, err := srv.buildCodexHookConfig(&store.Session{SessionID: "b1", Harness: msg.HarnessCodex})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	encoded, _ := json.Marshal(byEvent)
	var found bool
	var entries map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(encoded, &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, evEntries := range entries {
		for _, e := range evEntries {
			for _, hk := range e.Hooks {
				if !strings.Contains(hk.Command, "/hooks/exec/") {
					continue
				}
				found = true
				assertOneCurlRun(t, "codex user hook", hk.Command, "", "hook_01J0CODEX", "hooks", "exec", "")
			}
		}
	}
	if !found {
		t.Fatalf("no codex /hooks/exec/ command generated: %s", encoded)
	}
}
