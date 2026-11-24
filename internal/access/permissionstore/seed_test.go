package permissionstore

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

// seedRules is the rules a freshly migrated module starts with: the default
// rules migration 0002 inserts.
func seedRules(t *testing.T) []Rule {
	t.Helper()
	rules, err := newSeededTestStore(t).ListRules(testContext, "")
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

// TestANewModuleStartsWithTheDefaultRules pins what a new install gets: the
// eighteen seed-v1 rules, all global and enabled, stamped once.
func TestANewModuleStartsWithTheDefaultRules(t *testing.T) {
	rules := seedRules(t)
	if len(rules) != 18 {
		t.Fatalf("a new module holds %d rules, want the 18 defaults", len(rules))
	}
	for _, r := range rules {
		if !strings.HasPrefix(r.ID, "seed-v1-") || r.Scope != ScopeGlobal || !r.Enabled || r.CreatedAt == "" || r.CreatedAt != r.UpdatedAt {
			t.Fatalf("default rule %+v", r)
		}
		if _, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err != nil {
			t.Fatalf("rule %s is stamped %q, which is not RFC 3339: %v", r.ID, r.CreatedAt, err)
		}
	}
	deny, err := newSeededTestStore(t).GetRule(testContext, "seed-v1-deny-bash-rm-rf-abs")
	if err != nil || deny.Pattern != `^rm\s+-rf?\s+/` || deny.Priority != 50 || deny.Outcome != OutcomeDeny {
		t.Fatalf("the rm -rf rule = %+v, %v", deny, err)
	}
}

// TestSeedV1_RegexAllCompile is a sanity check — every seed pattern must
// be a valid RE2 regex, otherwise the engine silently skips the rule and
// the seed effectively isn't there.
func TestSeedV1_RegexAllCompile(t *testing.T) {
	for _, r := range seedRules(t) {
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
	for _, r := range seedRules(t) {
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
			resp, err := eng.Evaluate(testContext, EvaluateRequest{Tool: tc.tool, Input: input})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if resp.Outcome != tc.expected {
				t.Errorf("got %s, want %s (matched=%s, msg=%q)", resp.Outcome, tc.expected, resp.MatchedRuleID, resp.Message)
			}
		})
	}
}
