package permstore

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh DB in a temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "permission-store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func evaluate(t *testing.T, eng *Engine, tool, command string) EvaluateResponse {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"command": command})
	resp, err := eng.Evaluate(EvaluateRequest{
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
	if _, err := s.CreateRule(r); err != nil {
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
	resp, _ := eng.Evaluate(EvaluateRequest{InstanceID: "i1", Tool: "Bash", Input: input})
	if resp.Outcome != OutcomeDeny {
		t.Errorf("instance-scope rule must win over global, got %s", resp.Outcome)
	}

	// Different instance → falls through to global allow.
	resp2, _ := eng.Evaluate(EvaluateRequest{InstanceID: "i2", Tool: "Bash", Input: input})
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

	entries, err := s.ListAudit(ListAuditFilter{Limit: 10})
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
