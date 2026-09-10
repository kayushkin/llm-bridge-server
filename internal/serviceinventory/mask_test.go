package serviceinventory

import "testing"

// The rule is judged against column names measured across every live
// database on this host on 2026-09-10 (see mask.go). Each row here is one of
// those names, or a spelling of one.
func TestColumnHoldsCredential(t *testing.T) {
	masked := []string{
		"api_key", "apiKey", "APIKey", "token", "refresh_token", "access_token", "client_secret",
		"password", "password_hash", "runner_token_hash", "credentials", "private_key",
		"ssh_key", "passphrase", "bearer_token",
	}
	clear := []string{
		"id", "max_tokens", "input_tokens", "output_tokens", "token_count", "keyword", "keywords",
		"session_key", "window_key", "key", "credential_id", "api_key_id", "api_key_name", "api_key_hint",
		"api_key_status", "intended_app_match", "auth_type", "author", "author_id", "ssh_key_path",
		"apiauth_account", "token_type",
	}
	for _, name := range masked {
		if !ColumnHoldsCredential("things", name) {
			t.Errorf("%q: want masked", name)
		}
	}
	for _, name := range clear {
		if ColumnHoldsCredential("things", name) {
			t.Errorf("%q: want clear", name)
		}
	}
}

// apiauth's tokens.db keeps the secret in tokens.access and tokens.refresh;
// auth-store's credentials table keeps a label and a provider beside its
// secrets. The table name masks the value columns and nothing else.
func TestColumnHoldsCredentialReadsTheTableName(t *testing.T) {
	for _, c := range []struct {
		table, column string
		want          bool
	}{
		{"tokens", "access", true},
		{"tokens", "refresh", true},
		{"tokens", "provider", false},
		{"tokens", "expires_at", false},
		{"credentials", "label", false},
		{"credentials", "value", true},
		{"usage", "access", false},
	} {
		if got := ColumnHoldsCredential(c.table, c.column); got != c.want {
			t.Errorf("ColumnHoldsCredential(%q, %q) = %v, want %v", c.table, c.column, got, c.want)
		}
	}
}
