package signin

// Sign in with Discord.
//
// A person who has linked their Discord account signs in by proving they hold
// it: GET /discord/start sends the browser to Discord, Discord sends it back
// to GET /discord/callback with a code, and this service trades the code for
// the Discord user id. principal-store's identity binding (provider discord,
// empty tenant, the user id as subject) says which principal that is. No
// binding means no sign-in: Discord proves who someone is on Discord, and only
// a link that person made themselves says who they are here.
//
// Linking is the same round trip with purpose=link, started by someone
// already signed in. It binds the Discord account they prove they hold to
// their own principal, recorded as linked by themselves. It never links
// anyone else, and an account already linked to someone is refused.
//
// The app is the operator's Event-Manager Discord application, shared with
// discord-signup-store. Discord sends a browser back only to an address
// registered on the application, so the configured redirect URL must be in
// its OAuth2 redirect list. Only the user id is read (scope identify); the
// access token is used once and dropped.
//
// Each round trip is an attempt row: the state Discord carries and a
// browser-binding cookie, both kept only as hashes, ten minutes to finish, and
// spent once. A callback whose browser does not hold the attempt's cookie is
// refused, so a link to the callback cannot be finished in someone else's
// browser.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

const (
	// MethodDiscord is a session signed in with Discord.
	MethodDiscord = "discord"

	discordAttemptLifetime   = 10 * time.Minute
	discordAttemptCookieName = "sign_in_discord_attempt"

	discordPurposeSignIn = "sign_in"
	discordPurposeLink   = "link"

	// The error codes a failed round trip sends back to return_to as
	// ?sign_in_error=.
	discordErrorDenied        = "discord_denied"
	discordErrorAttempt       = "discord_attempt_not_usable"
	discordErrorUnavailable   = "discord_unavailable"
	discordErrorNotLinked     = "discord_not_linked"
	discordErrorMayNotSignIn  = "may_not_sign_in"
	discordErrorAlreadyLinked = "discord_already_linked"
	discordErrorSignedOut     = "signed_out_while_linking"
)

// Where Discord is. Variables only so tests can point them at a stand-in.
var (
	discordAuthorizeURL = "https://discord.com/oauth2/authorize"
	discordAPIBase      = "https://discord.com/api"
)

// DiscordConfig is the Discord application. ApplicationID empty switches
// Sign in with Discord off: its routes answer 503.
type DiscordConfig struct {
	ApplicationID            string
	ClientSecretCredentialID string
	RedirectURL              string
}

func (c DiscordConfig) configured() bool {
	return c.ApplicationID != "" && c.ClientSecretCredentialID != "" && c.RedirectURL != ""
}

// discordAttempt is one row of discord_attempts.
type discordAttempt struct {
	seq                int64
	browserSecretHash  string
	purpose            string
	linkingPrincipalID string
	linkingSessionID   string
	returnTo           string
}

func (s *Store) insertDiscordAttempt(ctx context.Context, stateHash, browserSecretHash, purpose, linkingPrincipalID, linkingSessionID, returnTo string) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO discord_attempts (state_sha256, browser_secret_sha256, purpose, linking_principal_id, linking_session_id, return_to, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, stateHash, browserSecretHash, purpose, linkingPrincipalID, linkingSessionID, returnTo,
		now.Unix(), now.Add(discordAttemptLifetime).Unix())
	return err
}

// spendDiscordAttempt marks the attempt with this state spent and returns it,
// or ErrNotFound when no unspent, unexpired attempt has it. It is one
// conditional update, so two callbacks with the same state cannot both spend
// it: the second finds consumed_at already set.
func (s *Store) spendDiscordAttempt(ctx context.Context, stateHash string) (discordAttempt, error) {
	now := s.now().Unix()
	var attempt discordAttempt
	err := s.db.QueryRowContext(ctx, `UPDATE discord_attempts SET consumed_at = $1
		WHERE state_sha256 = $2 AND consumed_at = 0 AND expires_at > $1
		RETURNING seq, browser_secret_sha256, purpose, linking_principal_id, linking_session_id, return_to`, now, stateHash).
		Scan(&attempt.seq, &attempt.browserSecretHash, &attempt.purpose, &attempt.linkingPrincipalID, &attempt.linkingSessionID, &attempt.returnTo)
	if errors.Is(err, sql.ErrNoRows) {
		return discordAttempt{}, ErrNotFound
	}
	if err != nil {
		return discordAttempt{}, err
	}
	return attempt, nil
}

// OpenSession records a session for a principal signed in some way other than
// a link, and returns it with its cookie secret.
func (s *Store) OpenSession(ctx context.Context, principalID, method string, facts SessionRequestFacts) (Session, string, error) {
	var session Session
	var secret string
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		session, secret, err = insertSession(ctx, tx, principalID, method, "", facts, s.now())
		return err
	})
	if err != nil {
		return Session{}, "", err
	}
	return session, secret, nil
}

// returnToOf checks where a round trip may end: an https address on the
// cookie domain or a name under it. Anything else would make the callback an
// open redirect. Empty means the sign-in page.
func (h *Handlers) returnToOf(raw string) (string, error) {
	if raw == "" {
		return h.config.SignInPageURL, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return "", fmt.Errorf("return_to %q must be an https address", raw)
	}
	host := parsed.Hostname()
	if host != h.config.CookieDomain && !strings.HasSuffix(host, "."+h.config.CookieDomain) {
		return "", fmt.Errorf("return_to %q is not on %s", raw, h.config.CookieDomain)
	}
	return parsed.String(), nil
}

// withQuery adds one query parameter to an address returnToOf accepted.
func withQuery(address, name, value string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return address
	}
	query := parsed.Query()
	query.Set(name, value)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// StartDiscord is GET /discord/start?purpose=sign_in|link&return_to=
func (h *Handlers) StartDiscord(w http.ResponseWriter, r *http.Request) {
	if !h.config.Discord.configured() {
		writeError(w, http.StatusServiceUnavailable, "discord_not_configured", "Sign in with Discord is not set up on this server")
		return
	}
	returnTo, err := h.returnToOf(r.URL.Query().Get("return_to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_return_to", err.Error())
		return
	}
	purpose := r.URL.Query().Get("purpose")
	if purpose == "" {
		purpose = discordPurposeSignIn
	}
	var linkingPrincipalID, linkingSessionID string
	switch purpose {
	case discordPurposeSignIn:
	case discordPurposeLink:
		session, principal, status, code := h.liveSessionOf(r)
		if status != http.StatusOK {
			writeError(w, status, code, "sign in first, then link your Discord account")
			return
		}
		linkingPrincipalID, linkingSessionID = principal.ID, session.ID
	default:
		writeError(w, http.StatusBadRequest, "invalid_purpose", "purpose is sign_in or link")
		return
	}
	state, stateHash, err := newSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	browserSecret, browserSecretHash, err := newSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	if err := h.store.insertDiscordAttempt(r.Context(), stateHash, browserSecretHash, purpose, linkingPrincipalID, linkingSessionID, returnTo); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: discordAttemptCookieName, Value: browserSecret, Path: "/", MaxAge: int(discordAttemptLifetime / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	query := url.Values{"client_id": {h.config.Discord.ApplicationID}, "redirect_uri": {h.config.Discord.RedirectURL},
		"response_type": {"code"}, "scope": {"identify"}, "state": {state}}
	http.Redirect(w, r, discordAuthorizeURL+"?"+query.Encode(), http.StatusFound)
}

// FinishDiscord is GET /discord/callback?code=&state= (or ?error=)
func (h *Handlers) FinishDiscord(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: discordAttemptCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	query := r.URL.Query()
	attempt, err := h.store.spendDiscordAttempt(r.Context(), hashSecret(query.Get("state")))
	if err != nil {
		// No attempt means no trusted return_to: go to the sign-in page.
		log.Printf("[discord] callback with no usable attempt: %v", err)
		http.Redirect(w, r, withQuery(h.config.SignInPageURL, "sign_in_error", discordErrorAttempt), http.StatusFound)
		return
	}
	fail := func(code string, detail error) {
		log.Printf("[discord] %s attempt failed: %s: %v", attempt.purpose, code, detail)
		http.Redirect(w, r, withQuery(attempt.returnTo, "sign_in_error", code), http.StatusFound)
	}
	cookie, err := r.Cookie(discordAttemptCookieName)
	if err != nil || hashSecret(cookie.Value) != attempt.browserSecretHash {
		fail(discordErrorAttempt, errors.New("the browser does not hold this attempt's cookie"))
		return
	}
	if query.Get("error") != "" {
		fail(discordErrorDenied, fmt.Errorf("discord answered error %q", query.Get("error")))
		return
	}
	discordUserID, err := h.discordUserIDOfCode(r.Context(), query.Get("code"))
	if err != nil {
		fail(discordErrorUnavailable, err)
		return
	}
	if attempt.purpose == discordPurposeLink {
		h.linkDiscord(w, r, attempt, discordUserID, fail)
		return
	}
	principal, err := h.directory.PrincipalOfIdentity(r.Context(), "discord", "", discordUserID)
	if errors.Is(err, ErrNotFound) {
		fail(discordErrorNotLinked, fmt.Errorf("no principal has Discord user %s", discordUserID))
		return
	}
	if err != nil {
		fail(discordErrorUnavailable, err)
		return
	}
	if err := principal.refusalToSignIn(); err != nil {
		fail(discordErrorMayNotSignIn, err)
		return
	}
	session, secret, err := h.store.OpenSession(r.Context(), principal.ID, MethodDiscord, SessionRequestFacts{UserAgent: r.UserAgent(), RemoteAddress: clientAddress(r)})
	if err != nil {
		fail(discordErrorUnavailable, err)
		return
	}
	h.setSessionCookie(w, secret, session.ExpiresAt)
	log.Printf("[discord] %s (%s) signed in as %s with Discord user %s", principal.ID, principal.DisplayName, session.ID, discordUserID)
	http.Redirect(w, r, attempt.returnTo, http.StatusFound)
}

// linkDiscord binds the proven Discord account to the principal that started
// the link, if that session is still live.
func (h *Handlers) linkDiscord(w http.ResponseWriter, r *http.Request, attempt discordAttempt, discordUserID string, fail func(string, error)) {
	session, err := h.store.GetSession(r.Context(), attempt.linkingSessionID)
	if err != nil || session.RevokedAt != 0 || h.store.now().Unix() >= session.ExpiresAt || session.PrincipalID != attempt.linkingPrincipalID {
		fail(discordErrorSignedOut, fmt.Errorf("session %s that started the link is not live: %v", attempt.linkingSessionID, err))
		return
	}
	note := fmt.Sprintf("linked by the person themselves while signed in as %s, proving the Discord account through OAuth", session.ID)
	err = h.directory.LinkIdentity(r.Context(), attempt.linkingPrincipalID, "discord", "", discordUserID, attempt.linkingPrincipalID, note)
	if errors.Is(err, ErrIdentityAlreadyBound) {
		fail(discordErrorAlreadyLinked, err)
		return
	}
	if err != nil {
		fail(discordErrorUnavailable, err)
		return
	}
	log.Printf("[discord] %s linked Discord user %s while signed in as %s", attempt.linkingPrincipalID, discordUserID, session.ID)
	http.Redirect(w, r, withQuery(attempt.returnTo, "discord_linked", "1"), http.StatusFound)
}

// discordUserIDOfCode trades an authorization code for the Discord user id.
// The access token is used for that one read and never kept.
func (h *Handlers) discordUserIDOfCode(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", errors.New("discord sent no code")
	}
	clientSecret, err := h.directory.resolveCredential(ctx, h.config.Discord.ClientSecretCredentialID, "trade a Sign in with Discord code")
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {h.config.Discord.ApplicationID}, "client_secret": {clientSecret}, "grant_type": {"authorization_code"},
		"code": {code}, "redirect_uri": {h.config.Discord.RedirectURL}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, discordAPIBase+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := h.directory.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("discord token exchange: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discord token exchange answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
		return "", errors.New("discord token exchange answered without an access token")
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := h.directory.getJSON(ctx, discordAPIBase+"/users/@me", map[string]string{"Authorization": "Bearer " + token.AccessToken}, &user); err != nil {
		return "", fmt.Errorf("discord /users/@me: %w", err)
	}
	if user.ID == "" {
		return "", errors.New("discord /users/@me answered without an id")
	}
	return user.ID, nil
}
