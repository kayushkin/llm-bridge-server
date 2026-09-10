package serviceinventory

import (
	"strings"
	"unicode"
)

// Words that mark a column as holding a credential. Matched against the
// column name split into words on "_", "-", "." and camelCase boundaries,
// lowercased — so `api_key`, `apiKey`, `refresh_token`, `client_secret`,
// `password_hash` and `runner_token_hash` are masked, while `max_tokens`,
// `input_tokens`, `keyword`, `keywords`, `session_key` and `window_key` are
// not.
//
// "key" on its own is deliberately not enough: measured across every live
// database on this host on 2026-09-10, bare "key" names a lookup key
// (`sessions.key`, `window_key`, `session_key`, `api_key_id`, `api_key_name`)
// far more often than a secret. It masks only beside a qualifier — api, access,
// private, secret, signing, ssh.
var credentialWords = map[string]bool{
	"token":       true,
	"tokens":      false, // plural is a count (input_tokens), never a secret
	"secret":      true,
	"secrets":     true,
	"password":    true,
	"passwd":      true,
	"pwd":         true,
	"credential":  true,
	"credentials": true,
	"bearer":      true,
	"cookie":      true,
	"apikey":      true,
	"passphrase":  true,
}

var keyQualifiers = map[string]bool{
	"api": true, "access": true, "private": true, "secret": true, "signing": true, "ssh": true,
}

// Words that say the column holds a reference to a credential, not the
// credential. A trailing one un-masks: `credential_id`, `api_key_id`,
// `api_key_name`, `api_key_hint`, `api_key_status`, `token_count`.
var referenceWords = map[string]bool{
	"id": true, "ids": true, "name": true, "hint": true, "status": true, "count": true,
	"type": true, "mode": true, "path": true, "kind": true, "at": true, "match": true,
}

// Tables whose name says every row is a credential, and the columns in such
// a table that hold the credential itself rather than describe it: apiauth's
// tokens.db keeps the access token in `tokens.access` and the refresh token in
// `tokens.refresh`, names the column rule alone cannot see.
var credentialTableWords = map[string]bool{
	"token": true, "tokens": true, "secret": true, "secrets": true, "credential": true, "credentials": true,
	"password": true, "passwords": true, "apikey": true, "apikeys": true,
}

var credentialValueColumns = map[string]bool{
	"access": true, "refresh": true, "value": true, "data": true, "blob": true, "hash": true, "raw": true,
}

// ColumnHoldsCredential reports whether a column's name — read with its
// table's — says its values are secrets and must be masked.
func ColumnHoldsCredential(table, column string) bool {
	if columnNameHoldsCredential(column) {
		return true
	}
	for _, w := range splitWords(table) {
		if credentialTableWords[w] {
			return credentialValueColumns[strings.ToLower(column)]
		}
	}
	return false
}

func columnNameHoldsCredential(column string) bool {
	words := splitWords(column)
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]
	if referenceWords[last] {
		return false
	}
	for i, w := range words {
		if credentialWords[w] {
			return true
		}
		if w == "key" && i > 0 && keyQualifiers[words[i-1]] {
			return true
		}
	}
	return false
}

// splitWords lowercases and splits on separators and camelCase boundaries.
func splitWords(name string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(runes[i-1]):
			flush()
			current = append(current, r)
		default:
			current = append(current, r)
		}
	}
	flush()
	return words
}
