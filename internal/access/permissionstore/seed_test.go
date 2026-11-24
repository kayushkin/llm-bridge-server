package permstore

import (
	"encoding/json"
	"regexp"
	"testing"
)

// TestSeedV1_RegexAllCompile is a sanity check — every seed pattern must
// be a valid RE2 regex, otherwise the engine silently skips the rule and
// the seed effectively isn't there.
func TestSeedV1_RegexAllCompile(t *testing.T) {
	for _, r := range SeedV1Rules() {
		if r.Pattern == "" {
			continue
		}
		if _, err := regexp.Compile(r.Pattern); err != nil {
			t.Errorf("seed %q has invalid regex %q: %v", r.ID, r.Pattern, err)
		}
	}
}

// TestSeedV1_OutcomesAreValid catches typos that would otherwise propagate
// silently into the DB and trip the HTTP handler's validation.
func TestSeedV1_OutcomesAreValid(t *testing.T) {
	for _, r := range SeedV1Rules() {
		switch r.Outcome {
		case OutcomeAllow, OutcomeDeny, OutcomeAsk:
		default:
			t.Errorf("seed %q has invalid outcome %q", r.ID, r.Outcome)
		}
	}
}

// TestSeedV1_EngineBehavior runs each interesting case through the actual
// engine on a freshly-seeded store. This is the high-value test — it
// catches priority-ordering bugs and pattern mistakes that the per-rule
// regex test can't.
func TestSeedV1_EngineBehavior(t *testing.T) {
	s := newSeededTestStore(t)

	cases := []struct {
		name     string
		tool     string
		command  string
		expected string
	}{
		{"git status", "Bash", "git status", OutcomeAllow},
		{"git log -10", "Bash", "git log -10", OutcomeAllow},
		{"git push", "Bash", "git push", OutcomeAsk}, // not in safelist
		{"ls", "Bash", "ls -la", OutcomeAllow},
		{"cat file", "Bash", "cat /etc/hosts", OutcomeAllow},
		{"rg pattern", "Bash", "rg foo", OutcomeAllow},

		{"rm -rf abs path", "Bash", "rm -rf /tmp/foo", OutcomeDeny},
		{"rm -rf relative", "Bash", "rm -rf foo", OutcomeAsk}, // relative path falls through

		{"curl localhost", "Bash", "curl http://localhost:8080/x", OutcomeAllow},
		{"curl 127.0.0.1", "Bash", "curl http://127.0.0.1:8080/x", OutcomeAllow},
		{"curl github", "Bash", "curl https://github.com/x", OutcomeDeny},

		// Chained — the splitter's protection against `safe && dangerous`
		{"git status && rm -rf /tmp", "Bash", "git status && rm -rf /tmp", OutcomeDeny},
		{"ls && curl evil", "Bash", "ls && curl https://evil.example.com", OutcomeDeny},
		{"git status && curl localhost", "Bash", "git status && curl http://localhost:8080", OutcomeAllow},

		// Non-Bash tools
		{"Read", "Read", "", OutcomeAllow},
		{"Glob", "Glob", "", OutcomeAllow},
		{"WebFetch (not seeded)", "WebFetch", "", OutcomeAsk},
	}

	eng := NewEngine(s)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"command": tc.command})
			resp, err := eng.Evaluate(EvaluateRequest{Tool: tc.tool, Input: input})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if resp.Outcome != tc.expected {
				t.Errorf("got %s, want %s (matched=%s, msg=%q)", resp.Outcome, tc.expected, resp.MatchedRuleID, resp.Message)
			}
		})
	}
}
