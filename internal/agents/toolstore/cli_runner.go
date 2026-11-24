package toolstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/kayushkin/tool-store/internal/childprocess"
)

// runCLI executes a kind=cli tool. The CLISpec.ArgsTemplate may contain
// {{key}} placeholders that get substituted with values from the JSON-decoded
// input. Substitution is literal (one placeholder = one argv element) — there
// is no shell expansion or word splitting, so no injection vector. Missing
// keys return an error rather than substituting the empty string.
func runCLI(ctx context.Context, t *Tool, inputJSON string) (string, error) {
	if t.CLI == nil || t.CLI.Command == "" {
		return "", fmt.Errorf("cli spec missing for tool %q", t.Name)
	}

	var input map[string]any
	if inputJSON != "" {
		if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
			return "", fmt.Errorf("invalid input json: %w", err)
		}
	}

	args := make([]string, 0, len(t.CLI.ArgsTemplate))
	for _, a := range t.CLI.ArgsTemplate {
		sub, err := substitute(a, input)
		if err != nil {
			return "", fmt.Errorf("tool %q: %w", t.Name, err)
		}
		args = append(args, sub)
	}

	if t.CLI.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t.CLI.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	cmd := childprocess.NewCommand(ctx, t.CLI.Command, args...)
	if t.CLI.WorkingDir != "" {
		cmd.Dir = t.CLI.WorkingDir
	}
	if len(t.EnvKeys) > 0 {
		env := os.Environ()
		for _, k := range t.EnvKeys {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
		cmd.Env = env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String()
	if err != nil {
		if stderr.Len() > 0 {
			return out, fmt.Errorf("%w: %s", err, stderr.String())
		}
		return out, err
	}
	return out, nil
}

var placeholderRE = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// substitute replaces {{key}} placeholders in s with input[key]. Each
// placeholder must resolve to a value in input — missing keys are an error,
// not silently substituted with "".
func substitute(s string, input map[string]any) (string, error) {
	matches := placeholderRE.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}
	var out bytes.Buffer
	prev := 0
	for _, m := range matches {
		// m: [fullStart fullEnd keyStart keyEnd]
		out.WriteString(s[prev:m[0]])
		key := s[m[2]:m[3]]
		v, ok := input[key]
		if !ok {
			return "", fmt.Errorf("missing input field %q for placeholder %s", key, s[m[0]:m[1]])
		}
		out.WriteString(stringify(v))
		prev = m[1]
	}
	out.WriteString(s[prev:])
	return out.String(), nil
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
