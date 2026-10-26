package server

// Peers: other llm-bridge-servers linked to this one.
//
// A link has two ends and each server keeps only its own (store/peers.go).
// On the issuing end an administrator makes a peer invite for a
// principal-store principal of kind peer and hands its one-time passphrase to
// the other server's operator. On the calling end that operator posts the
// passphrase to POST /peer-links, which redeems it at POST /peer/redeem on the
// issuing server, keeps the token it gets in auth-store and records the link.
// From then on the calling server sends the token as X-LLM-Bridge-Peer-Token,
// and request authorization treats the request as the peer principal: its
// grants, its groups and its administrator flag decide what it may do, and
// principal-store marks a peer restricted to its grants, so a link nobody has
// granted anything gives nothing.
//
// Two rules keep this from opening a hole:
//
//   - Demo login must be off on the issuing server. A server that takes peers
//     is reachable from another machine, and demo login signs anyone in as any
//     human by naming an id. Making an invite and redeeming one both refuse
//     while it is on.
//   - A peer link's URL must be https, and the certificate is verified with
//     the system's roots. There is no switch to skip that.
//
// Design note d107154a-49ac-471b-a1b7-fc7975de37f3.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

const (
	// peerSecretRandomBytes is the size of a peer invite passphrase and of a
	// peer token before encoding.
	peerSecretRandomBytes = 32
	// peerInviteDefaultLifetime is how long an invite may be redeemed when the
	// administrator does not say. An invite is handed from one operator to
	// another by hand, so an hour is time to paste it, not time to lose it.
	peerInviteDefaultLifetime = time.Hour
	// peerInviteLongestLifetime caps what an administrator may ask for.
	peerInviteLongestLifetime = 7 * 24 * time.Hour
	// peerCredentialProvider is the auth-store provider a peer link's token
	// is kept under.
	peerCredentialProvider = "llm-bridge-peer"
	// peerRedeemPath and peerSelfPath are this server's own routes, which the
	// calling end calls on the issuing end.
	peerRedeemPath = "/peer/redeem"
	peerSelfPath   = "/peer/self"
)

// hashPeerSecret is how a passphrase or a token is kept and looked up: never
// the value itself.
func hashPeerSecret(value string) string {
	return sha256Hex(value)
}

// peerInviteRequest is the body of POST /peer-invites.
type peerInviteRequest struct {
	PeerPrincipalID  string `json:"peer_principal_id"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

// peerInviteAnswer is the answer to POST /peer-invites: the invite and its
// passphrase, which is shown here once and kept nowhere.
type peerInviteAnswer struct {
	Invite     store.PeerInvite `json:"invite"`
	Passphrase string           `json:"passphrase"`
}

// peerRedeemRequest is the body of POST /peer/redeem.
type peerRedeemRequest struct {
	Passphrase string `json:"passphrase"`
}

// peerRedeemAnswer is the answer to POST /peer/redeem: the token, shown here
// once, and what it acts as.
type peerRedeemAnswer struct {
	Token           string `json:"token"`
	PeerTokenID     string `json:"peer_token_id"`
	PeerPrincipalID string `json:"peer_principal_id"`
}

// peerSelfAnswer is the answer to GET /peer/self: who a peer token acts as on
// this server.
type peerSelfAnswer struct {
	PeerTokenID     string `json:"peer_token_id"`
	PeerPrincipalID string `json:"peer_principal_id"`
	IsAdministrator bool   `json:"is_administrator"`
}

// peerRevokeRequest is the body of POST /peer-tokens/{id}/revoke and
// POST /peer-links/{id}/remove.
type peerRevokeRequest struct {
	Reason string `json:"reason"`
}

// peerLinkRequest is the body of POST /peer-links.
type peerLinkRequest struct {
	BaseURL     string `json:"base_url"`
	Passphrase  string `json:"passphrase"`
	DisplayName string `json:"display_name"`
}

// refuseWhileDemoLoginIsOn answers 409 and returns true while demo login is
// switched on. what says which peer step was refused.
func (s *Server) refuseWhileDemoLoginIsOn(w http.ResponseWriter, what string) bool {
	if !s.cfg.DemoLoginEnabled {
		return false
	}
	writeJSONError(w, http.StatusConflict, "demo_login_enabled", fmt.Sprintf(
		"%s is refused while demo login is on: a server that takes peers is reachable from another machine, and demo login signs anyone in as any human by naming an id. Set LLMBRIDGE_DEMO_LOGIN_ENABLED=false (people sign in through Entra ID) and restart", what))
	return true
}

// administratorMakingPeerChange returns the principal an administrator's
// change to a peer invite, token or link is recorded as.
func administratorMakingPeerChange(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID, identified := principalIdentityOfRequest(r)
	if !identified {
		writeJSONError(w, http.StatusForbidden, "peer_change_requires_a_principal", fmt.Sprintf(
			"a peer invite, token or link records which administrator changed it; send %s with the service token", principalIdentityHeader))
		return "", false
	}
	return principalID, true
}

// handleCreatePeerInvite: POST /peer-invites. The principal must be a peer in
// principal-store, active, and the invite expires.
func (s *Server) handleCreatePeerInvite(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	if s.refuseWhileDemoLoginIsOn(w, "making a peer invite") {
		return
	}
	var request peerInviteRequest
	if !decodeAdministrationBody(w, r, &request, "created_by") {
		return
	}
	lifetime := peerInviteDefaultLifetime
	if request.ExpiresInSeconds != 0 {
		lifetime = time.Duration(request.ExpiresInSeconds) * time.Second
	}
	if lifetime <= 0 || lifetime > peerInviteLongestLifetime {
		writeJSONError(w, http.StatusBadRequest, "invalid_expiry", fmt.Sprintf(
			"expires_in_seconds must be between 1 and %d, or left out for %d", int64(peerInviteLongestLifetime/time.Second), int64(peerInviteDefaultLifetime/time.Second)))
		return
	}
	if !isWellFormedPrincipalID(request.PeerPrincipalID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_principal_id", fmt.Sprintf(
			"peer_principal_id %q is not a principal-store id (principal_000001); create a principal of kind %q there first", request.PeerPrincipalID, principalclient.KindPeer))
		return
	}
	principal, err := s.principalClient.Get(r.Context(), request.PeerPrincipalID)
	switch {
	case errors.Is(err, principalclient.ErrNotFound):
		writeJSONError(w, http.StatusBadRequest, "unknown_principal", err.Error())
		return
	case err != nil:
		writeJSONError(w, http.StatusBadGateway, "principal_store_unavailable", err.Error())
		return
	case principal.Kind != principalclient.KindPeer:
		writeJSONError(w, http.StatusBadRequest, "principal_not_a_peer", fmt.Sprintf(
			"%s is a %q principal; a peer invite is made for a principal of kind %q, so that what the other server may do is a peer's and not a person's", principal.ID, principal.Kind, principalclient.KindPeer))
		return
	case principal.DisabledAt != 0:
		writeJSONError(w, http.StatusBadRequest, "principal_disabled", fmt.Sprintf("%s is disabled in principal-store", principal.ID))
		return
	}
	passphrase, err := randomURLSafeToken(peerSecretRandomBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	invite := store.PeerInvite{
		PassphraseHash:  hashPeerSecret(passphrase),
		PeerPrincipalID: principal.ID,
		CreatedBy:       administratorID,
		CreatedAt:       now,
		ExpiresAt:       now.Add(lifetime),
	}
	if err := s.store.InsertPeerInvite(&invite); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(peerInviteAnswer{Invite: invite, Passphrase: passphrase})
}

// handleListPeerInvites: GET /peer-invites.
func (s *Server) handleListPeerInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := s.store.ListPeerInvites()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, invites)
}

// handleRedeemPeerInvite: POST /peer/redeem. Open, because the calling server
// has no credential yet; the passphrase is the credential, and it works once.
func (s *Server) handleRedeemPeerInvite(w http.ResponseWriter, r *http.Request) {
	if s.refuseWhileDemoLoginIsOn(w, "redeeming a peer invite") {
		return
	}
	var request peerRedeemRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Passphrase == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "send {\"passphrase\": \"…\"}, the passphrase from POST /peer-invites")
		return
	}
	token, err := randomURLSafeToken(peerSecretRandomBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	issued, err := s.store.RedeemPeerInvite(hashPeerSecret(request.Passphrase), hashPeerSecret(token), time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrPeerInviteNotFound):
		writeJSONError(w, http.StatusUnauthorized, "peer_invite_unknown", "no peer invite has this passphrase")
		return
	case errors.Is(err, store.ErrPeerInviteAlreadyRedeemed):
		writeJSONError(w, http.StatusConflict, "peer_invite_already_redeemed", err.Error())
		return
	case errors.Is(err, store.ErrPeerInviteExpired):
		writeJSONError(w, http.StatusGone, "peer_invite_expired", err.Error())
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(peerRedeemAnswer{Token: token, PeerTokenID: issued.ID, PeerPrincipalID: issued.PeerPrincipalID})
}

// handlePeerSelf: GET /peer/self. Tells a peer who its token acts as here,
// which is how the calling end checks a link works.
func (s *Server) handlePeerSelf(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOfRequest(r)
	if !ok || caller.peerTokenID == "" {
		writeJSONError(w, http.StatusBadRequest, "not_a_peer", fmt.Sprintf("GET %s answers a caller presenting %s", peerSelfPath, peerTokenHeader))
		return
	}
	writeJSON(w, peerSelfAnswer{PeerTokenID: caller.peerTokenID, PeerPrincipalID: caller.principalID, IsAdministrator: caller.isAdministrator})
}

// handleListPeerTokens: GET /peer-tokens.
func (s *Server) handleListPeerTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListPeerTokens()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, tokens)
}

// handleRevokePeerToken: POST /peer-tokens/{id}/revoke. The other server's
// next request with the token is 401.
func (s *Server) handleRevokePeerToken(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request peerRevokeRequest
	if !decodeAdministrationBody(w, r, &request, "revoked_by") {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why the token is revoked; the reason stays on the row")
		return
	}
	token, err := s.store.RevokePeerToken(r.PathValue("id"), administratorID, request.Reason, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrPeerTokenNotFound):
		writeJSONError(w, http.StatusNotFound, "peer_token_not_found", "no peer token has id "+r.PathValue("id"))
		return
	case errors.Is(err, store.ErrPeerTokenAlreadyRevoked):
		writeJSONError(w, http.StatusConflict, "peer_token_already_revoked", fmt.Sprintf("%s was revoked at %s by %s: %s",
			token.ID, token.RevokedAt.UTC().Format(time.RFC3339), token.RevokedBy, token.RevokeReason))
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, token)
}

// peerBaseURLOf checks a peer link's address: https, a host, and nothing
// after the path a server is mounted at. It returns it without a trailing
// slash.
func peerBaseURLOf(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("base_url %q is not a URL: %v", raw, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("base_url %q must be https: a peer token crosses the network in every request, and there is no switch to send it in the clear", raw)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("base_url %q must be https://host[:port][/path] with no user, query or fragment", raw)
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// peerHTTPClient calls other servers. It verifies certificates with the
// system's roots, which is Go's default and is never switched off here, and it
// follows no redirect, which would carry the peer token to another host.
var peerHTTPClient = &http.Client{Timeout: 20 * time.Second, CheckRedirect: refusePeerRedirect}

// handleCreatePeerLink: POST /peer-links. Redeems the passphrase on the other
// server, keeps the token in auth-store and records the link. If auth-store or
// the store refuses after the other server issued the token, the answer names
// the token id so its operator can revoke it.
func (s *Server) handleCreatePeerLink(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request peerLinkRequest
	if !decodeAdministrationBody(w, r, &request, "created_by") {
		return
	}
	baseURL, err := peerBaseURLOf(request.BaseURL)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_base_url", err.Error())
		return
	}
	displayName := strings.TrimSpace(request.DisplayName)
	if displayName == "" || request.Passphrase == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "display_name and passphrase are both required")
		return
	}
	redeemed, err := redeemPeerInviteOn(r.Context(), baseURL, request.Passphrase)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "peer_redeem_failed", err.Error())
		return
	}
	credential, err := s.authClient.Create(r.Context(), authstoreclient.CredentialInput{
		Provider:    peerCredentialProvider,
		Owner:       "llm-bridge-server",
		Account:     redeemed.PeerPrincipalID,
		Label:       displayName,
		IntendedApp: "llm-bridge-server",
		AuthType:    "token",
		RefreshMode: "none",
		Token:       redeemed.Token,
		BaseURL:     baseURL,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "auth_store_failed", fmt.Sprintf(
			"%s issued peer token %s, and auth-store would not keep it, so no link was recorded: %v. Ask that server's operator to revoke %s", baseURL, redeemed.PeerTokenID, err, redeemed.PeerTokenID))
		return
	}
	link := store.PeerLink{
		BaseURL:               baseURL,
		DisplayName:           displayName,
		RemotePeerPrincipalID: redeemed.PeerPrincipalID,
		RemotePeerTokenID:     redeemed.PeerTokenID,
		CredentialID:          credential.ID,
		CreatedBy:             administratorID,
		CreatedAt:             time.Now().UTC(),
	}
	if err := s.store.InsertPeerLink(&link); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", fmt.Sprintf(
			"%s issued peer token %s and auth-store keeps it as credential %s, and the link could not be recorded: %v", baseURL, redeemed.PeerTokenID, credential.ID, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(link)
}

// redeemPeerInviteOn posts a passphrase to another server's POST /peer/redeem.
func redeemPeerInviteOn(ctx context.Context, baseURL, passphrase string) (peerRedeemAnswer, error) {
	body, err := json.Marshal(peerRedeemRequest{Passphrase: passphrase})
	if err != nil {
		return peerRedeemAnswer{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+peerRedeemPath, bytes.NewReader(body))
	if err != nil {
		return peerRedeemAnswer{}, fmt.Errorf("build POST %s%s: %w", baseURL, peerRedeemPath, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := peerHTTPClient.Do(request)
	if err != nil {
		return peerRedeemAnswer{}, fmt.Errorf("POST %s%s: %w", baseURL, peerRedeemPath, err)
	}
	defer response.Body.Close()
	answerBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return peerRedeemAnswer{}, fmt.Errorf("read %s%s's answer: %w", baseURL, peerRedeemPath, err)
	}
	if response.StatusCode != http.StatusCreated {
		return peerRedeemAnswer{}, fmt.Errorf("%s%s answered %s: %s", baseURL, peerRedeemPath, response.Status, strings.TrimSpace(string(answerBody)))
	}
	var answer peerRedeemAnswer
	if err := json.Unmarshal(answerBody, &answer); err != nil {
		return peerRedeemAnswer{}, fmt.Errorf("%s%s answered 201 with a body that is not a peer token: %w", baseURL, peerRedeemPath, err)
	}
	if answer.Token == "" || answer.PeerTokenID == "" || answer.PeerPrincipalID == "" {
		return peerRedeemAnswer{}, fmt.Errorf("%s%s answered 201 without a token, its id or its principal", baseURL, peerRedeemPath)
	}
	return answer, nil
}

// handleListPeerLinks: GET /peer-links.
func (s *Server) handleListPeerLinks(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.ListPeerLinks()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, links)
}

// handleCheckPeerLink: POST /peer-links/{id}/check. Asks the other server who
// this link's token acts as there, and passes its answer back unchanged.
func (s *Server) handleCheckPeerLink(w http.ResponseWriter, r *http.Request) {
	link, err := s.store.GetPeerLink(r.PathValue("id"))
	if errors.Is(err, store.ErrPeerLinkNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_link_not_found", "no peer link has id "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if link.RemovedAt != nil {
		writeJSONError(w, http.StatusConflict, "peer_link_removed", fmt.Sprintf("%s was removed at %s", link.ID, link.RemovedAt.UTC().Format(time.RFC3339)))
		return
	}
	response, err := s.callPeer(r.Context(), link, http.MethodGet, peerSelfPath, nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "peer_unreachable", err.Error())
		return
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
}

// callPeer sends one request to the server a link points at, as the peer
// principal the link's token acts as there. The token is read from auth-store
// on each call, so a credential rotated there is used at once.
func (s *Server) callPeer(ctx context.Context, link store.PeerLink, method, path string, body io.Reader) (*http.Response, error) {
	return s.callPeerWithHeaders(ctx, peerHTTPClient, link, method, path, body, nil)
}

// callPeerWithHeaders is callPeer through the given client, with
// setRequestHeaders, when set, adding headers before the peer token is set.
func (s *Server) callPeerWithHeaders(ctx context.Context, client *http.Client, link store.PeerLink, method, path string, body io.Reader, setRequestHeaders func(*http.Request)) (*http.Response, error) {
	resolved, err := s.authClient.Resolve(ctx, link.CredentialID, "peer link "+link.ID+" calls "+link.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("read peer link %s's token from auth-store credential %s: %w", link.ID, link.CredentialID, err)
	}
	token := resolved.Secret()
	if token == "" {
		return nil, fmt.Errorf("auth-store credential %s holds no token for peer link %s", link.CredentialID, link.ID)
	}
	request, err := http.NewRequestWithContext(ctx, method, link.BaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s%s: %w", method, link.BaseURL, path, err)
	}
	if setRequestHeaders != nil {
		setRequestHeaders(request)
	}
	request.Header.Set(peerTokenHeader, token)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s%s: %w", method, link.BaseURL, path, err)
	}
	return response, nil
}

// handleRemovePeerLink: POST /peer-links/{id}/remove. Stamps the link removed
// and deletes its token from auth-store. The token still works on the other
// server until its operator revokes it there; the answer says which.
func (s *Server) handleRemovePeerLink(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request peerRevokeRequest
	if !decodeAdministrationBody(w, r, &request, "removed_by") {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why the link is removed; the reason stays on the row")
		return
	}
	link, err := s.store.RemovePeerLink(r.PathValue("id"), administratorID, request.Reason, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrPeerLinkNotFound):
		writeJSONError(w, http.StatusNotFound, "peer_link_not_found", "no peer link has id "+r.PathValue("id"))
		return
	case errors.Is(err, store.ErrPeerLinkAlreadyRemoved):
		writeJSONError(w, http.StatusConflict, "peer_link_already_removed", fmt.Sprintf("%s was removed at %s by %s", link.ID, link.RemovedAt.UTC().Format(time.RFC3339), link.RemovedBy))
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if err := s.authClient.Delete(r.Context(), link.CredentialID); err != nil {
		writeJSONError(w, http.StatusBadGateway, "auth_store_failed", fmt.Sprintf(
			"%s is removed, and auth-store credential %s that holds its token could not be deleted: %v", link.ID, link.CredentialID, err))
		return
	}
	writeJSON(w, map[string]any{
		"link": link,
		"note": fmt.Sprintf("the token still works on %s until its operator revokes peer token %s there", link.BaseURL, link.RemotePeerTokenID),
	})
}
