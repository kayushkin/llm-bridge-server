// Package codexsubscription calls the OpenAI Responses API using the ChatGPT
// subscription credential that the Codex CLI stores on this host, rather than a
// pay-per-token API key.
//
// Why this exists: the same models are reachable two ways. An API key bills a
// balance; the subscription credential draws on the ChatGPT plan quota, which on
// this host sits close to unused. The endpoint below is the one the Codex CLI
// itself talks to.
//
// Read this before depending on it. This is not a documented, supported API:
//
//   - The endpoint is internal to the Codex product. It can change or start
//     rejecting third-party callers without notice or deprecation.
//   - The Codex source defines an `x-oai-attestation` header and an
//     AttestationProvider the host implements. The open-source CLI ships no
//     implementation and the server does not currently demand one, so calls like
//     ours are accepted. That is the mechanism that would bind requests to an
//     official client, and it is dormant rather than absent.
//   - The credential is issued for the Codex product, not for general API use.
//
// So treat a working call as a fact about today, not a contract. Anything built
// on this needs a path back to an API key.
package codexsubscription

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotSubscriptionAuth means the Codex CLI on this host is authenticated with
// an API key rather than a ChatGPT subscription, so there is no subscription
// credential to borrow.
var ErrNotSubscriptionAuth = errors.New("codex auth is not chatgpt subscription mode")

// ErrAccessTokenExpired means the stored token is past its expiry. The Codex CLI
// owns refresh; running any codex command refreshes the file in place.
var ErrAccessTokenExpired = errors.New("codex subscription access token expired")

// SubscriptionAccessToken is one bearer token read from the Codex CLI auth file,
// with the expiry decoded from its own claims.
type SubscriptionAccessToken struct {
	Value     string
	AccountID string
	ExpiresAt time.Time
	// LastRefresh is when the Codex CLI last rewrote the file. Carried for
	// diagnostics; expiry decisions use ExpiresAt.
	LastRefresh time.Time
}

// DefaultAuthFilePath returns the Codex CLI auth file location, honouring
// CODEX_HOME the same way the CLI does.
func DefaultAuthFilePath() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// ResolveSubscriptionAccessToken reads the Codex CLI auth file and returns the
// current access token.
//
// The auth file is deliberately the source of truth here rather than auth-store.
// The Codex CLI refreshes this token on its own schedule and writes it back to
// this file; a copy held anywhere else silently goes stale and then starts
// failing authentication. Callers should resolve per call, not cache.
func ResolveSubscriptionAccessToken(authFilePath string, now time.Time) (SubscriptionAccessToken, error) {
	raw, err := os.ReadFile(authFilePath)
	if err != nil {
		return SubscriptionAccessToken{}, fmt.Errorf("read codex auth file %s: %w", authFilePath, err)
	}

	var file struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return SubscriptionAccessToken{}, fmt.Errorf("decode codex auth file %s: %w", authFilePath, err)
	}

	if file.AuthMode != "chatgpt" {
		return SubscriptionAccessToken{}, fmt.Errorf("%w: auth_mode=%q in %s", ErrNotSubscriptionAuth, file.AuthMode, authFilePath)
	}
	if file.Tokens.AccessToken == "" {
		return SubscriptionAccessToken{}, fmt.Errorf("no access_token in %s", authFilePath)
	}

	expiresAt, err := accessTokenExpiry(file.Tokens.AccessToken)
	if err != nil {
		return SubscriptionAccessToken{}, err
	}
	if !now.Before(expiresAt) {
		return SubscriptionAccessToken{}, fmt.Errorf("%w at %s (now %s); run any codex command to refresh",
			ErrAccessTokenExpired, expiresAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}

	token := SubscriptionAccessToken{
		Value:     file.Tokens.AccessToken,
		AccountID: file.Tokens.AccountID,
		ExpiresAt: expiresAt,
	}
	if file.LastRefresh != "" {
		// A malformed timestamp here is not worth failing the call over: it is
		// diagnostic only, and expiry already came from the token itself.
		if parsed, parseErr := time.Parse(time.RFC3339, file.LastRefresh); parseErr == nil {
			token.LastRefresh = parsed
		}
	}
	return token, nil
}

// accessTokenExpiry reads the `exp` claim out of the access token.
//
// The token is a JWT, so it states its own expiry and we do not have to guess a
// lifetime or wait for a 401 to discover it went stale.
func accessTokenExpiry(accessToken string) (time.Time, error) {
	segments := strings.Split(accessToken, ".")
	if len(segments) != 3 {
		return time.Time{}, fmt.Errorf("access token is not a JWT: got %d segments, want 3", len(segments))
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode access token claims: %w", err)
	}
	var claims struct {
		Expiry int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("decode access token claims: %w", err)
	}
	if claims.Expiry == 0 {
		return time.Time{}, errors.New("access token has no exp claim")
	}
	return time.Unix(claims.Expiry, 0), nil
}
