package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The number resolveSessionBudget produced has to reach BOTH enforcers. Setting
// only the harness-native one caps Claude Code and leaves every other harness
// autoworker dispatches to — codex today — running with no ceiling at all.
func TestSpendCeilingReachesBothEnforcers(t *testing.T) {
	create := &msg.CreateSessionRequest{Harness: msg.HarnessCodex}
	if err := applySessionSpendCeiling(create, 4.25); err != nil {
		t.Fatalf("applySessionSpendCeiling: %v", err)
	}

	if create.MaxBudget == nil {
		t.Fatal("server-side ceiling is unset: bridge-server will never halt this session")
	}
	if *create.MaxBudget != 4.25 {
		t.Errorf("server-side ceiling: got %v, want 4.25", *create.MaxBudget)
	}

	var harnessConfig map[string]any
	if err := json.Unmarshal(create.HarnessConfig, &harnessConfig); err != nil {
		t.Fatalf("harness config is not an object: %v", err)
	}
	if harnessConfig["max_budget_usd"] != 4.25 {
		t.Errorf("harness-native ceiling: got %v, want 4.25", harnessConfig["max_budget_usd"])
	}
}

// 0 means "no ceiling" on both sides, so a zero must be written to neither.
// Passing it through would read as a cap of nothing and enforce nothing — the
// same 0-is-unlimited trap the fleet default is documented against.
func TestZeroCeilingIsWrittenNowhere(t *testing.T) {
	for name, ceiling := range map[string]float64{
		"zero":     0,
		"negative": -1,
	} {
		create := &msg.CreateSessionRequest{Harness: msg.HarnessClaudeCode}
		if err := applySessionSpendCeiling(create, ceiling); err != nil {
			t.Fatalf("%s: applySessionSpendCeiling: %v", name, err)
		}
		if create.MaxBudget != nil {
			t.Errorf("%s: wrote a server-side ceiling of %v; an unset ceiling must stay unset",
				name, *create.MaxBudget)
		}
		if create.HarnessConfig != nil {
			t.Errorf("%s: wrote a harness config %q; an unset ceiling must stay unset",
				name, string(create.HarnessConfig))
		}
	}
}

// The two ceilings must carry the same number. They enforce at different moments
// — the harness one mid-call, the server one at the next accounted event — and a
// session whose two ceilings disagree is capped at whichever fires first, which
// is not a number anybody chose.
func TestBothEnforcersCarryTheSameNumber(t *testing.T) {
	create := &msg.CreateSessionRequest{Harness: msg.HarnessClaudeCode}
	if err := applySessionSpendCeiling(create, 12.50); err != nil {
		t.Fatalf("applySessionSpendCeiling: %v", err)
	}

	var harnessConfig struct {
		MaxBudgetUSD float64 `json:"max_budget_usd"`
	}
	if err := json.Unmarshal(create.HarnessConfig, &harnessConfig); err != nil {
		t.Fatalf("harness config is not an object: %v", err)
	}
	if create.MaxBudget == nil || *create.MaxBudget != harnessConfig.MaxBudgetUSD {
		t.Errorf("the two ceilings disagree: server-side %v, harness-native %v",
			create.MaxBudget, harnessConfig.MaxBudgetUSD)
	}
}
