package server

import "testing"

// TestCarriesHarnessConfigSeesAnEmptyDisabledToolsList pins the nil/length
// distinction on the budget-only fast path.
//
// A request carrying max_budget and an empty disabled_tools list is two
// requests: move the ceiling, and re-enable every tool. The first is server
// state; the second has to reach the harness. Treating the empty list as
// "nothing to forward" answered the caller with the saved ceiling and dropped
// the tool change without a word.
func TestCarriesHarnessConfigSeesAnEmptyDisabledToolsList(t *testing.T) {
	budget := 5.0

	cases := []struct {
		name string
		req  ConfigSessionRequest
		want bool
	}{
		{
			name: "empty list re-enables every tool, so the harness must hear it",
			req:  ConfigSessionRequest{MaxBudget: &budget, DisabledTools: []string{}},
			want: true,
		},
		{
			name: "a named tool set reaches the harness",
			req:  ConfigSessionRequest{MaxBudget: &budget, DisabledTools: []string{"shell_commands"}},
			want: true,
		},
		{
			name: "budget only, saying nothing about tools, stays here",
			req:  ConfigSessionRequest{MaxBudget: &budget},
			want: false,
		},
		{
			name: "budget plus an explicit null tool set still says nothing about tools",
			req:  ConfigSessionRequest{MaxBudget: &budget, DisabledTools: nil},
			want: false,
		},
		{
			name: "a model change reaches the harness",
			req:  ConfigSessionRequest{MaxBudget: &budget, Model: "sonnet"},
			want: true,
		},
		{
			name: "an effort change reaches the harness",
			req:  ConfigSessionRequest{MaxBudget: &budget, Effort: "high"},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriesHarnessConfig(tc.req); got != tc.want {
				t.Fatalf("carriesHarnessConfig(%#v) = %v, want %v", tc.req, got, tc.want)
			}
		})
	}
}
