package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kayushkin/tool-store/internal/childprocess"
	"github.com/kayushkin/tool-store/schema"
)

// Shell returns a tool that executes shell commands via bash.
func Shell() Impl {
	return ShellWithSessionEnvironment(nil)
}

// ShellWithSessionEnvironment is Shell for a caller that serves many sessions
// from one process. sessionEnvironment returns NAME=value entries that belong
// to the session (for an llm-bridge session, the LLM_BRIDGE_ values that let
// bridge-share-file and bridge-button reach its own chat). It is called for
// every command, so a value the session's caller refreshed applies to the next
// command, and its entries come after the process's own, so they win. A nil
// sessionEnvironment adds nothing.
func ShellWithSessionEnvironment(sessionEnvironment func() []string) Impl {
	type input struct {
		Command  string   `json:"command"`
		Commands []string `json:"commands"`
		Workdir  string   `json:"workdir"`
	}
	return Impl{
		Name: "shell_commands",
		// The no-polling sentence is a cost control, and the schema is the only
		// place the model ever reads it: it sees the description on every turn
		// and it never sees a project's notes. It is here rather than in a
		// harness's prompt because the misuse belongs to this tool.
		//
		// Measured on the host that filed this, 2026-08-01: two
		// `until [ -s FILE ]; do sleep N; done` calls had been running for
		// 90,830s and 90,362s — over 25 hours each, ~15,000 iterations between
		// them — waiting on sentinel files that were zero bytes and stayed that
		// way. Nothing in this tool would ever have stopped them; Shell applies
		// no timeout of its own, so a waiting command waits for as long as its
		// caller's context lives.
		//
		// The sentence deliberately does not say "this tool has no timeout".
		// That is true of Shell itself but a consumer may wrap the context in
		// one, and a description that lies to the model in that harness is
		// worse than one that only states the rule. Priced against Anthropic's
		// count_tokens before landing: +52 tokens on the cached prefix, taking
		// this description from 37 to 89.
		Description: "Run one or more shell commands. Use commands[] array to run multiple in sequence (preferred). Each turn is expensive — maximize work per call. Never poll — no sleep, no until/while retry loop, no watch. A waiting command holds the session for as long as it waits. Check once and return; if it is not ready, check again on a later turn.",
		InputSchema: schema.Props([]string{}, map[string]any{
			"command":  schema.Str("Single shell command to execute"),
			"commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Multiple commands to run sequentially (preferred)"},
			"workdir":  schema.Str("Working directory (optional, defaults to cwd)"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			// Collect commands to run.
			cmds := in.Commands
			if in.Command != "" {
				if len(cmds) == 0 {
					cmds = []string{in.Command}
				} else {
					cmds = append([]string{in.Command}, cmds...)
				}
			}
			if len(cmds) == 0 {
				return "error: provide 'command' or 'commands'", nil
			}

			var results []string
			for i, c := range cmds {
				// A cancelled call must not keep starting the commands behind
				// the one it stopped. Say how many were skipped rather than
				// returning a run that looks complete.
				if err := ctx.Err(); err != nil {
					results = append(results, fmt.Sprintf("(stopped: %s — %d of %d commands not run)", err, len(cmds)-i, len(cmds)))
					break
				}
				cmd := childprocess.NewCommand(ctx, "bash", "-c", c)
				if in.Workdir != "" {
					cmd.Dir = in.Workdir
				}
				// Ensure common dev tools are on PATH (mise, go, node, etc.)
				cmd.Env = ensureDevToolsOnPath()
				if sessionEnvironment != nil {
					cmd.Env = append(cmd.Env, sessionEnvironment()...)
				}
				out, err := cmd.CombinedOutput()
				result := string(out)
				if err != nil {
					result = fmt.Sprintf("%s\nexit: %s", result, err)
				}
				if result == "" {
					result = "(no output)"
				}
				result = truncateShellOutput(result)
				if len(cmds) > 1 {
					results = append(results, fmt.Sprintf("=== $ %s ===\n%s", c, result))
				} else {
					results = append(results, result)
				}
			}
			return strings.Join(results, "\n\n"), nil
		},
	}
}

// truncateShellOutput caps shell output, keeping its head and its tail and
// reporting what it dropped in between. Every budget here counts CHARACTERS, so
// neither cut can land inside one and the omission notice reports the same unit
// it counted.
func truncateShellOutput(s string) string {
	const (
		maxLines  = 500
		maxChars  = 50000
		headLines = 250
		tailLines = 200
		headChars = 25000
		tailChars = 20000
	)

	// Check character limit first. Count characters, not bytes: the budget is
	// named for characters and the notice below reports in characters, so len()
	// would both cut a multi-byte character in half and overstate how much was
	// dropped. Measured on 60000 Japanese characters, the byte count announced
	// 135000 omitted when 44998 were.
	totalChars := utf8.RuneCountInString(s)
	if totalChars > maxChars {
		if totalChars <= headChars+tailChars {
			return s
		}
		head := headCharacters(s, headChars)
		tail := tailCharacters(s, tailChars)
		omitted := totalChars - headChars - tailChars
		return fmt.Sprintf("%s\n\n[... %d characters omitted ...]\n\n%s", head, omitted, tail)
	}

	// Check line limit
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}

	head := strings.Join(lines[:headLines], "\n")
	tail := strings.Join(lines[len(lines)-tailLines:], "\n")
	omitted := len(lines) - headLines - tailLines
	return fmt.Sprintf("%s\n\n[... %d lines omitted ...]\n\n%s", head, omitted, tail)
}

// headCharacters returns the first n characters of s, or all of s when it holds
// fewer. A cut counted in characters cannot land inside one, so the result is
// valid UTF-8 wherever s was.
func headCharacters(s string, n int) string {
	counted := 0
	for offset := range s {
		if counted >= n {
			return s[:offset]
		}
		counted++
	}
	return s
}

// tailCharacters returns the last n characters of s, or all of s when it holds
// fewer. Cutting by byte instead would leave the orphan trailing bytes of a
// split character at the START of the result, which breaks any consumer that
// scans forward from byte 0 looking for a character to begin.
func tailCharacters(s string, n int) string {
	skip := utf8.RuneCountInString(s) - n
	if skip <= 0 {
		return s
	}
	counted := 0
	for offset := range s {
		if counted == skip {
			return s[offset:]
		}
		counted++
	}
	return ""
}

// ensureDevToolsOnPath returns os.Environ() with mise shim paths prepended to PATH.
func ensureDevToolsOnPath() []string {
	env := os.Environ()
	home, _ := os.UserHomeDir()
	if home == "" {
		return env
	}

	// Paths where mise installs tool shims/binaries
	extraPaths := []string{
		filepath.Join(home, ".local", "share", "mise", "shims"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, "go", "bin"),
	}

	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			existing := e[5:]
			env[i] = "PATH=" + strings.Join(extraPaths, ":") + ":" + existing
			return env
		}
	}
	return env
}
