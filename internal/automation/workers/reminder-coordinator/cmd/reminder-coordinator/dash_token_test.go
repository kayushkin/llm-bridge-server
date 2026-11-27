package main

import "testing"

// `systemctl show -p Environment` prints ONE line whose value is the whole
// variable list, so the first whitespace-separated field is
// `Environment=FIRST_VAR=…`. Splitting the raw output on whitespace and asking
// each field for a `DASH_TOKEN=` prefix therefore cannot see the token when it
// is declared first. On this host DASH_PORT happens to come first, which is the
// only reason the original parse worked, and nothing guarantees that ordering.
func TestDashTokenIsFoundWhereverItSitsInTheUnit(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		output string
	}{
		{"declared first", "Environment=DASH_TOKEN=secret DASH_PORT=8101 HOME=/home/u\n"},
		{"declared in the middle", "Environment=DASH_PORT=8101 DASH_TOKEN=secret HOME=/home/u\n"},
		{"declared last", "Environment=DASH_PORT=8101 HOME=/home/u DASH_TOKEN=secret\n"},
		{"the only variable", "Environment=DASH_TOKEN=secret\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := dashTokenFromSystemdEnvironmentProperty(testCase.output)
			if err != nil {
				t.Fatalf("dashTokenFromSystemdEnvironmentProperty: %v", err)
			}
			if token != "secret" {
				t.Errorf("token = %q, want %q", token, "secret")
			}
		})
	}
}

// Loud, not silent: without this the mirror would 401 on every call and look
// like it simply had nothing to do.
func TestDashTokenRefusesToGuessWhenTheUnitDeclaresNone(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		output string
	}{
		{"no token among the variables", "Environment=DASH_PORT=8101 HOME=/home/u\n"},
		{"no variables at all", "Environment=\n"},
		{"a name that merely ends in the token's name", "Environment=EXTRA_DASH_TOKEN=secret\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := dashTokenFromSystemdEnvironmentProperty(testCase.output)
			if err == nil {
				t.Fatalf("got token %q, want a refusal", token)
			}
		})
	}
}
