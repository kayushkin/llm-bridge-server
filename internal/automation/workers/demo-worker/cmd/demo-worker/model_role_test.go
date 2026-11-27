package main

import (
	"strings"
	"testing"
)

// A run that would start sessions and holds no role refuses before it reads
// or writes anything.
func TestARunThatStartsSessionsNeedsAModelRole(t *testing.T) {
	for _, cfg := range []config{
		{Dispatch: true, EmailBoardID: "e", WorkBoardID: "w", MaxBudget: 1, KanbanURL: "http://127.0.0.1:1"},
		{Review: true, EmailBoardID: "e", WorkBoardID: "w", MaxBudget: 1, KanbanURL: "http://127.0.0.1:1"},
	} {
		if err := run(cfg); err == nil || !strings.Contains(err.Error(), "no model role") {
			t.Errorf("dispatch=%v review=%v: err = %v, want a refusal for the missing role", cfg.Dispatch, cfg.Review, err)
		}
	}
}
