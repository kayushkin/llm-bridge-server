package permissionstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testContext is the context every store call in these tests runs under.
var testContext = context.Background()

// newTestStore opens a fresh database and wipes the seed rules so
// engine unit tests start from a clean slate. Use newSeededTestStore when
// the seed defaults are what's under test.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := newSeededTestStore(t)
	if _, err := s.db.Exec(`DELETE FROM rules`); err != nil {
		t.Fatalf("wipe rules: %v", err)
	}
	return s
}

// newSeededTestStore gives the test a store on a Postgres database of its own
// with every migration applied — so the default seed rules are present.
func newSeededTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}

func evaluate(t *testing.T, eng *Engine, tool, command string) EvaluateResponse {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"command": command})
	resp, err := eng.Evaluate(testContext, EvaluateRequest{
		BridgeID: "test",
		Tool:     tool,
		Input:    input,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return resp
}

func mustCreateRule(t *testing.T, s *Store, r Rule) {
	t.Helper()
	if _, err := s.CreateRule(testContext, r); err != nil {
		t.Fatalf("create rule: %v", err)
	}
}

func TestEngine_NoRules_DefaultsToAsk(t *testing.T) {
	s := newTestStore(t)
	eng := NewEngine(s)
	resp := evaluate(t, eng, "Bash", "git status")
	if resp.Outcome != OutcomeAsk {
		t.Errorf("expected ask, got %s", resp.Outcome)
	}
	if resp.MatchedRuleID != RuleIDDefaultAsk {
		t.Errorf("expected default-ask, got %q", resp.MatchedRuleID)
	}
}

func TestEngine_AllowMatchingPattern(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "^git status", Outcome: OutcomeAllow, Enabled: true})
	eng := NewEngine(s)
	if resp := evaluate(t, eng, "Bash", "git status"); resp.Outcome != OutcomeAllow {
		t.Errorf("expected allow, got %s", resp.Outcome)
	}
}

func TestEngine_DenyShortCircuits(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Priority: 10, Tool: "Bash", Pattern: "^git", Outcome: OutcomeAllow, Enabled: true})
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Priority: 5, Tool: "Bash", Pattern: "^rm", Outcome: OutcomeDeny, Message: "no rm", Enabled: true})
	eng := NewEngine(s)

	// Mixed chain: git status (allow) && rm -rf foo (deny) → deny wins.
	resp := evaluate(t, eng, "Bash", "git status && rm -rf foo")
	if resp.Outcome != OutcomeDeny {
		t.Errorf("expected deny on chained call, got %s", resp.Outcome)
	}
	if resp.Message != "no rm" {
		t.Errorf("expected deny message propagated, got %q", resp.Message)
	}
}

func TestEngine_AskOnUnmatchedAtom(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "^git status", Outcome: OutcomeAllow, Enabled: true})
	eng := NewEngine(s)

	// First atom matches allow; second has no rule → cascade-ask.
	resp := evaluate(t, eng, "Bash", "git status && curl example.com")
	if resp.Outcome != OutcomeAsk {
		t.Errorf("expected ask, got %s", resp.Outcome)
	}
}

func TestEngine_PriorityOrdering(t *testing.T) {
	s := newTestStore(t)
	// Both match `git`; priority 1 (deny) wins over priority 5 (allow).
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Priority: 5, Tool: "Bash", Pattern: "^git", Outcome: OutcomeAllow, Enabled: true})
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Priority: 1, Tool: "Bash", Pattern: "^git", Outcome: OutcomeDeny, Enabled: true})
	eng := NewEngine(s)

	if resp := evaluate(t, eng, "Bash", "git status"); resp.Outcome != OutcomeDeny {
		t.Errorf("expected lower-priority rule to win (deny), got %s", resp.Outcome)
	}
}

func TestEngine_ScopeCascade_InstanceBeatsGlobal(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "^git", Outcome: OutcomeAllow, Enabled: true})
	mustCreateRule(t, s, Rule{Scope: ScopeInstancePfx + "i1", Tool: "Bash", Pattern: "^git", Outcome: OutcomeDeny, Enabled: true})
	eng := NewEngine(s)

	input, _ := json.Marshal(map[string]string{"command": "git status"})
	resp, _ := eng.Evaluate(testContext, EvaluateRequest{InstanceID: "i1", Tool: "Bash", Input: input})
	if resp.Outcome != OutcomeDeny {
		t.Errorf("instance-scope rule must win over global, got %s", resp.Outcome)
	}

	// Different instance → falls through to global allow.
	resp2, _ := eng.Evaluate(testContext, EvaluateRequest{InstanceID: "i2", Tool: "Bash", Input: input})
	if resp2.Outcome != OutcomeAllow {
		t.Errorf("non-matching instance scope should not apply, got %s", resp2.Outcome)
	}
}

func TestEngine_DisabledRuleSkipped(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "^git", Outcome: OutcomeDeny, Enabled: false})
	eng := NewEngine(s)
	if resp := evaluate(t, eng, "Bash", "git status"); resp.Outcome != OutcomeAsk {
		t.Errorf("disabled rule should not apply, got %s", resp.Outcome)
	}
}

func TestEngine_WildcardTool(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "*", Outcome: OutcomeAllow, Enabled: true})
	eng := NewEngine(s)
	if resp := evaluate(t, eng, "WebFetch", ""); resp.Outcome != OutcomeAllow {
		t.Errorf("wildcard tool rule should match WebFetch, got %s", resp.Outcome)
	}
}

func TestEngine_BadRegexSkipped(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "[invalid(", Outcome: OutcomeDeny, Enabled: true})
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "^git", Outcome: OutcomeAllow, Enabled: true, Priority: 100})
	eng := NewEngine(s)
	// Bad regex in deny rule should be skipped, allow rule wins.
	if resp := evaluate(t, eng, "Bash", "git status"); resp.Outcome != OutcomeAllow {
		t.Errorf("bad regex should be skipped, got %s", resp.Outcome)
	}
}

func TestEngine_AuditWritten(t *testing.T) {
	s := newTestStore(t)
	mustCreateRule(t, s, Rule{Scope: ScopeGlobal, Tool: "Bash", Pattern: "^git", Outcome: OutcomeAllow, Enabled: true})
	eng := NewEngine(s)
	evaluate(t, eng, "Bash", "git status")

	entries, err := s.ListAudit(testContext, ListAuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Outcome != OutcomeAllow || entries[0].Tool != "Bash" {
		t.Errorf("audit entry mismatch: %+v", entries[0])
	}
}
