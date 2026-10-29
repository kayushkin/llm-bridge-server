package server

// Peers: other llm-bridge-servers connected to this one (design note
// d107154a-49ac-471b-a1b7-fc7975de37f3, "Decided on 2026-10-27").
//
// A connection and a grant of access are separate.
//
//   - Connecting. An administrator on a server with an address mints a
//     one-time passphrase (POST /peer-connection-invites). An administrator
//     on the other server enters that address and passphrase (POST
//     /peer-connections), and this server calls the other's POST
//     /peer/connect. Each side records the connection and a credential for
//     connection calls; neither gets any access. When the entering server
//     gives its own https address both call each other directly; otherwise
//     it dials out and keeps a socket open (peer_socket.go), and requests in
//     both directions travel over that.
//   - Granting. An administrator on server X grants the other server a level
//     on X (POST /peer-connections/{id}/grant): X makes, or reuses, a
//     principal of kind peer for the other server, puts it at the level —
//     a group's member, an administrator, or neither — mints a peer token and
//     delivers it over the connection (POST /peer/access on the other side),
//     which keeps it in auth-store. The other server then presents it as
//     X-LLM-Bridge-Peer-Token, and request authorization treats the request as
//     that peer principal: its grants, groups and administrator flag decide.
//     principal-store marks a peer restricted to its grants, so a grant at
//     level none allows nothing. Revoking (POST
//     /peer-connections/{id}/revoke-grant) invalidates the token here and
//     tells the other side.
//
// Three rules keep this from opening a hole:
//
//   - Demo login must be off on a server that takes calls from another
//     machine: one that mints connection passphrases, accepts sockets, or
//     takes peer tokens and connection calls by https. Demo login signs
//     anyone in as any human by naming an id. A server whose only peer
//     traffic arrives over a socket it dialed out itself takes no inbound
//     connection from the network, so it may keep demo login on.
//   - An address must be https, verified with the system's roots, and no
//     peer client follows a redirect, which would carry a credential to
//     another host.
//   - Every change records the administrator who made it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

const (
	// peerSecretRandomBytes is the size of a passphrase, a connection
	// credential and a peer token before encoding.
	peerSecretRandomBytes = 32
	// peerInviteDefaultLifetime is how long a connection passphrase may be
	// redeemed when the administrator does not say. It is handed from one
	// operator to another by hand.
	peerInviteDefaultLifetime = time.Hour
	// peerInviteLongestLifetime caps what an administrator may ask for.
	peerInviteLongestLifetime = 7 * 24 * time.Hour
	// peerCredentialProvider is the auth-store provider a peer token this
	// server holds is kept under; peerConnectionCredentialProvider is the one
	// for the credential this server presents on connection calls.
	peerCredentialProvider           = "llm-bridge-peer"
	peerConnectionCredentialProvider = "llm-bridge-peer-connection"
	// peerConnectionCredentialHeader carries a connection's credential on
	// connection calls made by https and when opening a socket.
	peerConnectionCredentialHeader = "X-LLM-Bridge-Peer-Connection"

	peerConnectPath       = "/peer/connect"
	peerSocketPath        = "/peer/socket"
	peerAccessPath        = "/peer/access"
	peerAccessRevokedPath = "/peer/access/revoked"
	// peerConnectionRemovedPath is where a server tells the other it removed
	// their connection.
	peerConnectionRemovedPath = "/peer/connection/removed"
	peerSelfPath              = "/peer/self"
	peerServerNamePath        = "/peer/server-name"
)

// hashPeerSecret is how a passphrase, a credential or a token is kept and
// looked up: never the value itself.
func hashPeerSecret(value string) string {
	return sha256Hex(value)
}

// peerAccessLevelDefinition is a level that puts the peer in a group of this
// server's: what the group's grants are. The group itself is made by
// provisioning the level and found by its key in peer_access_levels.
type peerAccessLevelDefinition struct {
	key         string
	displayName string
	description string
	grants      []msg.PeerAccessLevelGrant
}

// peerAccessLevelsWithGroups are the levels this server can provision.
var peerAccessLevelsWithGroups = []peerAccessLevelDefinition{
	{
		key:         "run_sessions",
		displayName: "Run sessions",
		description: "Start sessions on any of this server's instances, as any agent, with any tool; see and drive only the sessions it started.",
		grants: []msg.PeerAccessLevelGrant{
			{Relation: "can_dispatch_on", ResourceType: "instance", ResourceID: grantclient.AllResources},
			{Relation: "can_run_as", ResourceType: "agent", ResourceID: grantclient.AllResources},
			{Relation: "can_use", ResourceType: "tool", ResourceID: grantclient.AllResources},
		},
	},
	{
		key:         peerMessagesLevelKey,
		displayName: "Send requests",
		description: "Send requests and notices that wait here for an administrator's approval, and nothing else: no session, catalog or store is reachable.",
		grants:      []msg.PeerAccessLevelGrant{},
	},
	{
		key:         "observe",
		displayName: "Observe",
		description: "Read every session on any of this server's instances, and the catalog; start, send to, stop or change nothing.",
		grants: []msg.PeerAccessLevelGrant{
			{Relation: observeSessionsRelation, ResourceType: "instance", ResourceID: grantclient.AllResources},
		},
	},
}

func peerAccessLevelDefinitionOf(key string) (peerAccessLevelDefinition, bool) {
	for _, level := range peerAccessLevelsWithGroups {
		if level.key == key {
			return level, true
		}
	}
	return peerAccessLevelDefinition{}, false
}

// refuseWhileDemoLoginIsOn answers 409 and returns true while demo login is
// switched on. what names the step refused.
func (s *Server) refuseWhileDemoLoginIsOn(w http.ResponseWriter, what string) bool {
	if !s.cfg.DemoLoginEnabled {
		return false
	}
	writeJSONError(w, http.StatusConflict, "demo_login_enabled", fmt.Sprintf(
		"%s is refused while demo login is on: it means taking calls from another machine, and demo login signs anyone in as any human by naming an id. Set LLMBRIDGE_DEMO_LOGIN_ENABLED=false and restart, or connect from this server to one with an address without giving this server's own", what))
	return true
}

// administratorMakingPeerChange returns the principal a change is recorded as.
func administratorMakingPeerChange(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID, identified := principalIdentityOfRequest(r)
	if !identified {
		writeJSONError(w, http.StatusForbidden, "peer_change_requires_a_principal", fmt.Sprintf(
			"a change to a peer connection records which administrator made it; send %s with the service token", principalIdentityHeader))
		return "", false
	}
	return principalID, true
}

// peerBaseURLOf checks an address: https, a host, and nothing after the path a
// server is mounted at. It returns it without a trailing slash.
func peerBaseURLOf(field, raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%s %q is not a URL: %v", field, raw, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("%s %q must be https: credentials cross the network in every request, and there is no switch to send them in the clear", field, raw)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s %q must be https://host[:port][/path] with no user, query or fragment", field, raw)
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

// peerHTTPClient makes connection calls to other servers. It verifies
// certificates with the system's roots, which is Go's default and is never
// switched off here, and follows no redirect.
var peerHTTPClient = &http.Client{Timeout: 20 * time.Second, CheckRedirect: refusePeerRedirect}

// refusePeerRedirect passes a redirect back as the answer instead of
// following it: Go would carry the credential headers to whatever host the
// redirect names.
func refusePeerRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// handleCreatePeerConnectionInvite: POST /peer-connection-invites.
func (s *Server) handleCreatePeerConnectionInvite(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	if s.refuseWhileDemoLoginIsOn(w, "making a peer connection passphrase") {
		return
	}
	var request msg.PeerConnectionInviteRequest
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
	passphrase, err := randomURLSafeToken(peerSecretRandomBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	invite := store.PeerConnectionInvite{
		PeerConnectionInvite: msg.PeerConnectionInvite{CreatedBy: administratorID, CreatedAt: now, ExpiresAt: now.Add(lifetime)},
		PassphraseHash:       hashPeerSecret(passphrase),
	}
	if err := s.store.InsertPeerConnectionInvite(&invite); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, msg.PeerConnectionInviteCreated{Invite: invite.PeerConnectionInvite, Passphrase: passphrase})
}

// handleListPeerConnectionInvites: GET /peer-connection-invites.
func (s *Server) handleListPeerConnectionInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := s.store.ListPeerConnectionInvites()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	records := make([]msg.PeerConnectionInvite, 0, len(invites))
	for _, invite := range invites {
		records = append(records, invite.PeerConnectionInvite)
	}
	writeJSON(w, records)
}

// handleCreatePeerConnection: POST /peer-connections. This server's
// administrator entered the other server's address and passphrase.
func (s *Server) handleCreatePeerConnection(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request msg.PeerConnectRequest
	if !decodeAdministrationBody(w, r, &request, "created_by") {
		return
	}
	remoteBaseURL, err := peerBaseURLOf("base_url", request.BaseURL)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_base_url", err.Error())
		return
	}
	displayName := strings.TrimSpace(request.DisplayName)
	ownServerName := s.thisServerName()
	// The other server shows this one by the name it reports; the typed label
	// is only a fallback, and an older server requires one, so this server's
	// own name stands in when none was typed.
	nameForThisServer := strings.TrimSpace(request.NameForThisServer)
	if nameForThisServer == "" {
		nameForThisServer = ownServerName
	}
	if request.Passphrase == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "passphrase is required")
		return
	}
	if nameForThisServer == "" {
		writeJSONError(w, http.StatusBadRequest, "this_server_has_no_name", fmt.Sprintf(
			"this server has no %s, so the other server would have nothing to call it: set it on the settings page, or send name_for_this_server", config.SettingServerName))
		return
	}
	transport := msg.PeerTransportDialOut
	thisServerBaseURL := ""
	if strings.TrimSpace(request.ThisServerBaseURL) != "" {
		thisServerBaseURL, err = peerBaseURLOf("this_server_base_url", request.ThisServerBaseURL)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_base_url", err.Error())
			return
		}
		if s.refuseWhileDemoLoginIsOn(w, "giving this server's own address to a peer") {
			return
		}
		transport = msg.PeerTransportDirect
	}
	inboundCredential, err := randomURLSafeToken(peerSecretRandomBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	var answer msg.PeerConnectAnswer
	if err := postJSONToPeer(r.Context(), remoteBaseURL+peerConnectPath, nil, msg.PeerConnectCall{
		Passphrase: request.Passphrase, ServerName: nameForThisServer, OwnServerName: ownServerName, BaseURL: thisServerBaseURL, Credential: inboundCredential,
	}, http.StatusCreated, &answer); err != nil {
		writeJSONError(w, http.StatusBadGateway, "peer_connect_failed", err.Error())
		return
	}
	if answer.ConnectionID == "" || answer.Credential == "" || answer.Transport != transport {
		writeJSONError(w, http.StatusBadGateway, "peer_connect_failed", fmt.Sprintf(
			"%s answered with connection %q, transport %q and a credential set: %t; this server asked for %s", remoteBaseURL, answer.ConnectionID, answer.Transport, answer.Credential != "", transport))
		return
	}
	otherServerName := strings.TrimSpace(answer.OwnServerName)
	if checkServerNameValue(otherServerName) != nil {
		log.Printf("[peers] %s reported a name that cannot be shown (%q); keeping none", remoteBaseURL, answer.OwnServerName)
		otherServerName = ""
	}
	credential, err := s.authClient.Create(r.Context(), authstoreclient.CredentialInput{
		Provider: peerConnectionCredentialProvider, Owner: "llm-bridge-server", Account: answer.ConnectionID, Label: firstNonEmpty(otherServerName, displayName, answer.ConnectionID),
		IntendedApp: "llm-bridge-server", AuthType: "token", RefreshMode: "none", Token: answer.Credential, BaseURL: remoteBaseURL,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "auth_store_failed", fmt.Sprintf(
			"%s recorded connection %s, and auth-store would not keep its credential, so nothing was recorded here: %v. Ask that server's administrator to remove %s", remoteBaseURL, answer.ConnectionID, err, answer.ConnectionID))
		return
	}
	connection := store.PeerConnection{
		PeerConnection: msg.PeerConnection{
			DisplayName: displayName, Transport: transport, Role: msg.PeerConnectionRoleInitiating, RemoteBaseURL: remoteBaseURL,
			RemoteConnectionID: answer.ConnectionID, CreatedBy: administratorID, CreatedAt: time.Now().UTC(),
		},
		InboundCredentialHash: hashPeerSecret(inboundCredential),
		OutboundCredentialID:  credential.ID,
	}
	if otherServerName != "" {
		reportedAt := connection.CreatedAt
		connection.OtherServerName, connection.OtherServerNameReportedAt = otherServerName, &reportedAt
	}
	if err := s.store.InsertPeerConnection(&connection); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", fmt.Sprintf(
			"%s recorded connection %s and auth-store keeps its credential as %s, and the connection could not be recorded here: %v", remoteBaseURL, answer.ConnectionID, credential.ID, err))
		return
	}
	if thisServerDials(connection) {
		s.startPeerDialer(connection)
	} else if connection.Transport == msg.PeerTransportDirect {
		// The connect call's body has no room for ids (an older server refuses
		// a field it does not know), and nothing else is sent until someone
		// uses the connection, so swap them now. A socket's dial does this on
		// a dial_out connection.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := s.exchangeServerIdentities(ctx, connection); err != nil {
				log.Printf("[peers] %s: swap server ids after connecting: %v", connection.ID, err)
			}
		}()
	}
	writeJSONStatus(w, http.StatusCreated, s.peerConnectionView(connection))
}

// handlePeerConnect: POST /peer/connect. Open, because the other server has
// no credential yet; the one-time passphrase is the credential.
func (s *Server) handlePeerConnect(w http.ResponseWriter, r *http.Request) {
	if s.refuseWhileDemoLoginIsOn(w, "accepting a peer connection") {
		return
	}
	var call msg.PeerConnectCall
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&call); err != nil || call.Passphrase == "" || call.Credential == "" ||
		(strings.TrimSpace(call.ServerName) == "" && strings.TrimSpace(call.OwnServerName) == "") {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "send the passphrase from POST /peer-connection-invites, own_server_name or server_name, and credential")
		return
	}
	otherServerName := strings.TrimSpace(call.OwnServerName)
	if checkServerNameValue(otherServerName) != nil {
		log.Printf("[peers] a connecting server reported a name that cannot be shown (%q); keeping none", call.OwnServerName)
		otherServerName = ""
	}
	label := firstNonEmpty(otherServerName, strings.TrimSpace(call.ServerName))
	transport := msg.PeerTransportDialOut
	remoteBaseURL := ""
	if call.BaseURL != "" {
		var err error
		if remoteBaseURL, err = peerBaseURLOf("base_url", call.BaseURL); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_base_url", err.Error())
			return
		}
		transport = msg.PeerTransportDirect
	}
	now := time.Now().UTC()
	passphraseHash := hashPeerSecret(call.Passphrase)
	invite, err := s.store.CheckPeerConnectionInvite(passphraseHash, now)
	if inviteError := peerConnectionInviteError(err); inviteError != nil {
		inviteError.write(w)
		return
	}
	credentialForThem, err := randomURLSafeToken(peerSecretRandomBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	outbound, err := s.authClient.Create(r.Context(), authstoreclient.CredentialInput{
		Provider: peerConnectionCredentialProvider, Owner: "llm-bridge-server", Account: label, Label: label,
		IntendedApp: "llm-bridge-server", AuthType: "token", RefreshMode: "none", Token: call.Credential, BaseURL: remoteBaseURL,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "auth_store_failed", fmt.Sprintf("auth-store would not keep the connection's credential, so no connection was made: %v", err))
		return
	}
	connection := store.PeerConnection{
		PeerConnection: msg.PeerConnection{
			DisplayName: strings.TrimSpace(call.ServerName), Transport: transport, Role: msg.PeerConnectionRoleAccepting, RemoteBaseURL: remoteBaseURL,
			CreatedBy: invite.CreatedBy, CreatedAt: now,
		},
		InboundCredentialHash: hashPeerSecret(credentialForThem),
		OutboundCredentialID:  outbound.ID,
	}
	if otherServerName != "" {
		reportedAt := now
		connection.OtherServerName, connection.OtherServerNameReportedAt = otherServerName, &reportedAt
	}
	if err := s.store.RedeemPeerConnectionInvite(passphraseHash, &connection, now); err != nil {
		if deleteErr := s.authClient.Delete(r.Context(), outbound.ID); deleteErr != nil {
			log.Printf("[peers] ERROR: delete auth-store credential %s after a failed connection: %v", outbound.ID, deleteErr)
		}
		if inviteError := peerConnectionInviteError(err); inviteError != nil {
			inviteError.write(w)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, msg.PeerConnectAnswer{ConnectionID: connection.ID, Transport: transport, Credential: credentialForThem, OwnServerName: s.thisServerName()})
}

// peerConnectionInviteError maps a passphrase check to its answer, or nil.
func peerConnectionInviteError(err error) *requestCredentialError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrPeerConnectionInviteNotFound):
		return &requestCredentialError{http.StatusUnauthorized, "peer_connection_invite_unknown", "no peer connection passphrase matches"}
	case errors.Is(err, store.ErrPeerConnectionInviteAlreadyRedeemed):
		return &requestCredentialError{http.StatusConflict, "peer_connection_invite_already_redeemed", err.Error()}
	case errors.Is(err, store.ErrPeerConnectionInviteExpired):
		return &requestCredentialError{http.StatusGone, "peer_connection_invite_expired", err.Error()}
	default:
		return &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
	}
}

// peerConnectionView is a connection with what is true of it now.
func (s *Server) peerConnectionView(connection store.PeerConnection) msg.PeerConnectionView {
	socket := s.peerSockets.socketOf(connection.ID)
	view := msg.PeerConnectionView{Connection: connection.PeerConnection, SocketConnected: socket != nil,
		OtherServerName: connection.OtherServerName, OtherServerNameReportedAt: connection.OtherServerNameReportedAt,
		OtherServerID: connection.OtherServerID}
	if socket != nil {
		answering, lastAnsweredAt, notAnsweringSince, lastProbeFailure := socket.health()
		view.Answering = &answering
		view.LastAnsweredAt = &lastAnsweredAt
		if !answering {
			view.NotAnsweringSince = &notAnsweringSince
			view.LastProbeFailure = lastProbeFailure
		}
	}
	if grant, err := s.store.LivePeerGrantOfConnection(connection.ID); err == nil {
		view.AccessGiven = &grant.PeerGrant
	} else if !errors.Is(err, store.ErrPeerGrantNotFound) {
		log.Printf("[peers] ERROR: read %s's grant: %v", connection.ID, err)
	}
	if access, err := s.store.LivePeerAccessOfConnection(connection.ID); err == nil {
		view.AccessHeld = &access
	} else if !errors.Is(err, store.ErrPeerAccessNotFound) {
		log.Printf("[peers] ERROR: read %s's access: %v", connection.ID, err)
	}
	return view
}

// handleListPeerConnections: GET /peer-connections.
func (s *Server) handleListPeerConnections(w http.ResponseWriter, r *http.Request) {
	connections, err := s.store.ListPeerConnections()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	views := make([]msg.PeerConnectionView, 0, len(connections))
	for _, connection := range connections {
		views = append(views, s.peerConnectionView(connection))
	}
	writeJSON(w, views)
}

// livePeerConnectionOf reads the connection a route's {id} names and answers
// 404 or 409 itself when it is missing or removed.
func (s *Server) livePeerConnectionOf(w http.ResponseWriter, r *http.Request) (store.PeerConnection, bool) {
	connection, err := s.store.GetPeerConnection(r.PathValue("id"))
	if errors.Is(err, store.ErrPeerConnectionNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_connection_not_found", "no peer connection has id "+r.PathValue("id"))
		return store.PeerConnection{}, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return store.PeerConnection{}, false
	}
	if connection.RemovedAt != nil {
		writeJSONError(w, http.StatusConflict, "peer_connection_removed", fmt.Sprintf("%s was removed at %s by %s: %s",
			connection.ID, connection.RemovedAt.UTC().Format(time.RFC3339), connection.RemovedBy, connection.RemoveReason))
		return store.PeerConnection{}, false
	}
	return connection, true
}

// handleGetPeerConnection: GET /peer-connections/{id}.
func (s *Server) handleGetPeerConnection(w http.ResponseWriter, r *http.Request) {
	connection, err := s.store.GetPeerConnection(r.PathValue("id"))
	if errors.Is(err, store.ErrPeerConnectionNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_connection_not_found", "no peer connection has id "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, s.peerConnectionView(connection))
}

// handleListPeerAccessLevels: GET /peer-access-levels.
func (s *Server) handleListPeerAccessLevels(w http.ResponseWriter, r *http.Request) {
	levels := []msg.PeerAccessLevel{
		{Key: msg.PeerAccessLevelNone, DisplayName: "No access", Description: "The other server holds a token that acts as a peer principal with nothing granted, so every request it makes is refused."},
	}
	for _, definition := range peerAccessLevelsWithGroups {
		level := msg.PeerAccessLevel{Key: definition.key, DisplayName: definition.displayName, Description: definition.description, Grants: definition.grants, NeedsGroup: true}
		groupID, err := s.store.PeerAccessLevelGroup(definition.key)
		switch {
		case err == nil:
			level.GroupPrincipalID = groupID
		case !errors.Is(err, store.ErrPeerAccessLevelNotProvisioned):
			writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
			return
		}
		levels = append(levels, level)
	}
	levels = append(levels,
		msg.PeerAccessLevel{Key: msg.PeerAccessLevelAdministrator, DisplayName: "Administrator", Description: "Everything, operator routes included: principal-store marks the peer an administrator."},
		msg.PeerAccessLevel{Key: msg.PeerAccessLevelGroup, DisplayName: "A group's grants", Description: "Whatever the principal-store group named in group_principal_id is granted.", NeedsGroup: true},
	)
	writeJSON(w, levels)
}

// handleProvisionPeerAccessLevel: POST /peer-access-levels/{key}/provision.
// Makes the level's group in principal-store and its grants in grant-store,
// and records the group's id under the level's key.
func (s *Server) handleProvisionPeerAccessLevel(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	definition, known := peerAccessLevelDefinitionOf(r.PathValue("key"))
	if !known {
		writeJSONError(w, http.StatusNotFound, "peer_access_level_not_found", fmt.Sprintf("%q is not a level that needs provisioning; GET /peer-access-levels lists them", r.PathValue("key")))
		return
	}
	if groupID, err := s.store.PeerAccessLevelGroup(definition.key); err == nil {
		writeJSONError(w, http.StatusConflict, "peer_access_level_already_provisioned", fmt.Sprintf("%s is provisioned as group %s", definition.key, groupID))
		return
	} else if !errors.Is(err, store.ErrPeerAccessLevelNotProvisioned) {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	groupID, err := s.principalClient.Create(r.Context(), principalclient.KindGroup, "Linked servers: "+strings.ToLower(definition.displayName))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "principal_store_failed", err.Error())
		return
	}
	for _, levelGrant := range definition.grants {
		if _, err := s.grantClient.Create(r.Context(), grantclient.GrantInput{
			PrincipalID: groupID, Relation: levelGrant.Relation, ResourceType: levelGrant.ResourceType, ResourceID: levelGrant.ResourceID,
			Note: "peer access level " + definition.key + ", provisioned by " + administratorID,
		}); err != nil {
			writeJSONError(w, http.StatusBadGateway, "grant_store_failed", fmt.Sprintf("group %s was made and a grant failed, so the level is not recorded: %v", groupID, err))
			return
		}
	}
	if err := s.store.RecordPeerAccessLevelGroup(definition.key, groupID, administratorID, time.Now().UTC()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, msg.PeerAccessLevel{Key: definition.key, DisplayName: definition.displayName, Description: definition.description,
		Grants: definition.grants, GroupPrincipalID: groupID, NeedsGroup: true})
}

// peerLevelEffect is what a level does to the peer principal: the group it
// joins, and whether principal-store marks it an administrator.
type peerLevelEffect struct {
	groupPrincipalID string
	isAdministrator  bool
}

// peerLevelEffectOf resolves a grant request to its effect, or the answer
// that refuses it.
func (s *Server) peerLevelEffectOf(ctx context.Context, request msg.PeerGrantRequest) (peerLevelEffect, *requestCredentialError) {
	switch request.Level {
	case msg.PeerAccessLevelNone:
		return peerLevelEffect{}, nil
	case msg.PeerAccessLevelAdministrator:
		return peerLevelEffect{isAdministrator: true}, nil
	case msg.PeerAccessLevelGroup:
		if request.GroupPrincipalID == "" {
			return peerLevelEffect{}, &requestCredentialError{http.StatusBadRequest, "group_principal_id_required", "level group needs group_principal_id"}
		}
		group, err := s.principalClient.Get(ctx, request.GroupPrincipalID)
		switch {
		case errors.Is(err, principalclient.ErrNotFound):
			return peerLevelEffect{}, &requestCredentialError{http.StatusBadRequest, "unknown_principal", err.Error()}
		case err != nil:
			return peerLevelEffect{}, &requestCredentialError{http.StatusBadGateway, "principal_store_unavailable", err.Error()}
		case group.Kind != principalclient.KindGroup:
			return peerLevelEffect{}, &requestCredentialError{http.StatusBadRequest, "principal_not_a_group", fmt.Sprintf("%s is a %q, not a group", group.ID, group.Kind)}
		case group.DisabledAt != 0:
			return peerLevelEffect{}, &requestCredentialError{http.StatusBadRequest, "principal_disabled", group.ID + " is disabled"}
		}
		return peerLevelEffect{groupPrincipalID: group.ID}, nil
	}
	if request.GroupPrincipalID != "" {
		return peerLevelEffect{}, &requestCredentialError{http.StatusBadRequest, "group_principal_id_not_accepted", "group_principal_id goes only with level group"}
	}
	if _, known := peerAccessLevelDefinitionOf(request.Level); !known {
		return peerLevelEffect{}, &requestCredentialError{http.StatusBadRequest, "unknown_peer_access_level", fmt.Sprintf("%q is not a level; GET /peer-access-levels lists them", request.Level)}
	}
	groupID, err := s.store.PeerAccessLevelGroup(request.Level)
	if errors.Is(err, store.ErrPeerAccessLevelNotProvisioned) {
		return peerLevelEffect{}, &requestCredentialError{http.StatusConflict, "peer_access_level_not_provisioned", fmt.Sprintf(
			"level %s has no group on this server yet; POST /peer-access-levels/%s/provision makes it", request.Level, request.Level)}
	}
	if err != nil {
		return peerLevelEffect{}, &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
	}
	return peerLevelEffect{groupPrincipalID: groupID}, nil
}

// applyPeerLevelEffect makes the peer principal what the level says.
func (s *Server) applyPeerLevelEffect(ctx context.Context, peerPrincipalID string, effect peerLevelEffect) error {
	defer s.principalLookupCache.forget(peerPrincipalID)
	if err := s.principalClient.SetAdministrator(ctx, peerPrincipalID, effect.isAdministrator); err != nil {
		return err
	}
	if effect.groupPrincipalID != "" {
		return s.principalClient.AddMember(ctx, effect.groupPrincipalID, peerPrincipalID)
	}
	return nil
}

// undoPeerLevelEffect takes away what a grant's level gave the peer principal.
func (s *Server) undoPeerLevelEffect(ctx context.Context, grant store.PeerGrant) error {
	defer s.principalLookupCache.forget(grant.PeerPrincipalID)
	if grant.Level == msg.PeerAccessLevelAdministrator {
		if err := s.principalClient.SetAdministrator(ctx, grant.PeerPrincipalID, false); err != nil {
			return err
		}
	}
	if grant.GroupPrincipalID != "" {
		return s.principalClient.RemoveMember(ctx, grant.GroupPrincipalID, grant.PeerPrincipalID)
	}
	return nil
}

// peerPrincipalOfConnection returns the principal this server made for the
// other server, making it on the first grant.
func (s *Server) peerPrincipalOfConnection(ctx context.Context, connection store.PeerConnection) (string, error) {
	if connection.PeerPrincipalID != "" {
		return connection.PeerPrincipalID, nil
	}
	principalID, err := s.principalClient.Create(ctx, principalclient.KindPeer, connection.DisplayName)
	if err != nil {
		return "", err
	}
	if err := s.store.SetPeerConnectionPeerPrincipal(connection.ID, principalID); err != nil {
		return "", fmt.Errorf("principal-store made %s for %s and it could not be recorded on the connection: %w", principalID, connection.ID, err)
	}
	return principalID, nil
}

// handleGrantPeerAccess: POST /peer-connections/{id}/grant. A live grant on
// the connection is revoked first, so the connection gives one level at a
// time. A grant the other server does not confirm is revoked again, so no
// token exists that nobody holds.
func (s *Server) handleGrantPeerAccess(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request msg.PeerGrantRequest
	if !decodeAdministrationBody(w, r, &request, "created_by") {
		return
	}
	connection, ok := s.livePeerConnectionOf(w, r)
	if !ok {
		return
	}
	effect, refusal := s.peerLevelEffectOf(r.Context(), request)
	if refusal != nil {
		refusal.write(w)
		return
	}
	peerPrincipalID, err := s.peerPrincipalOfConnection(r.Context(), connection)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "principal_store_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	if previous, err := s.store.LivePeerGrantOfConnection(connection.ID); err == nil {
		if _, err := s.store.RevokePeerGrant(previous.ID, administratorID, "replaced by a grant at level "+request.Level, now); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
			return
		}
		if err := s.undoPeerLevelEffect(r.Context(), previous); err != nil {
			writeJSONError(w, http.StatusBadGateway, "principal_store_failed", fmt.Sprintf("%s is revoked and its level could not be taken back from %s: %v", previous.ID, peerPrincipalID, err))
			return
		}
	} else if !errors.Is(err, store.ErrPeerGrantNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if err := s.applyPeerLevelEffect(r.Context(), peerPrincipalID, effect); err != nil {
		writeJSONError(w, http.StatusBadGateway, "principal_store_failed", err.Error())
		return
	}
	token, err := randomURLSafeToken(peerSecretRandomBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "random_source_failed", err.Error())
		return
	}
	grant := store.PeerGrant{
		PeerGrant: msg.PeerGrant{ConnectionID: connection.ID, PeerPrincipalID: peerPrincipalID, Level: request.Level,
			GroupPrincipalID: effect.groupPrincipalID, CreatedBy: administratorID, CreatedAt: now},
		TokenHash: hashPeerSecret(token),
	}
	if err := s.store.InsertPeerGrant(&grant); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	deliveryErr := s.callPeerConnection(r.Context(), connection, http.MethodPost, peerAccessPath, msg.PeerAccessDelivery{
		GrantID: grant.ID, PeerPrincipalID: peerPrincipalID, Level: grant.Level, Token: token,
	}, http.StatusCreated, nil)
	if deliveryErr != nil {
		if _, err := s.store.RevokePeerGrant(grant.ID, administratorID, "the other server did not take it: "+deliveryErr.Error(), time.Now().UTC()); err != nil {
			log.Printf("[peers] ERROR: revoke undelivered %s: %v", grant.ID, err)
		}
		if err := s.undoPeerLevelEffect(r.Context(), grant); err != nil {
			log.Printf("[peers] ERROR: take back undelivered %s's level from %s: %v", grant.ID, peerPrincipalID, err)
		}
		writeJSONError(w, http.StatusBadGateway, "peer_access_not_delivered", fmt.Sprintf("%s was made and the other server did not take it, so it is revoked: %v", grant.ID, deliveryErr))
		return
	}
	delivered := time.Now().UTC()
	if err := s.store.MarkPeerGrantDelivered(grant.ID, delivered); err != nil {
		log.Printf("[peers] ERROR: mark %s delivered: %v", grant.ID, err)
	}
	grant.DeliveredAt = &delivered
	writeJSONStatus(w, http.StatusCreated, grant.PeerGrant)
}

// handleRevokePeerGrant: POST /peer-connections/{id}/revoke-grant. The grant
// is revoked here whatever happens next; the answer says whether the other
// server was told.
func (s *Server) handleRevokePeerGrant(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request msg.PeerReasonRequest
	if !decodeAdministrationBody(w, r, &request, "revoked_by") {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why; the reason stays on the row")
		return
	}
	connection, err := s.store.GetPeerConnection(r.PathValue("id"))
	if errors.Is(err, store.ErrPeerConnectionNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_connection_not_found", "no peer connection has id "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	revoked, notice, failure := s.revokeLivePeerGrant(r.Context(), connection, administratorID, request.Reason)
	if failure != nil {
		failure.write(w)
		return
	}
	writeJSON(w, msg.PeerGrantRevoked{Grant: revoked.PeerGrant, Notice: notice})
}

// revokeLivePeerGrant revokes the connection's live grant, takes its level
// back, and tells the other server, saying in notice whether it heard.
func (s *Server) revokeLivePeerGrant(ctx context.Context, connection store.PeerConnection, administratorID, reason string) (store.PeerGrant, string, *requestCredentialError) {
	return s.revokeLivePeerGrantTelling(ctx, connection, administratorID, reason, true)
}

// revokeLivePeerGrantTelling is revokeLivePeerGrant, telling the other server
// only when tell is set: a server the other side has just told it removed the
// connection has no one left to tell.
func (s *Server) revokeLivePeerGrantTelling(ctx context.Context, connection store.PeerConnection, administratorID, reason string, tell bool) (store.PeerGrant, string, *requestCredentialError) {
	live, err := s.store.LivePeerGrantOfConnection(connection.ID)
	if errors.Is(err, store.ErrPeerGrantNotFound) {
		return store.PeerGrant{}, "", &requestCredentialError{http.StatusNotFound, "peer_grant_not_found", connection.ID + " gives the other server no access"}
	}
	if err != nil {
		return store.PeerGrant{}, "", &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
	}
	revoked, err := s.store.RevokePeerGrant(live.ID, administratorID, reason, time.Now().UTC())
	if err != nil {
		return store.PeerGrant{}, "", &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
	}
	if err := s.undoPeerLevelEffect(ctx, revoked); err != nil {
		return store.PeerGrant{}, "", &requestCredentialError{http.StatusBadGateway, "principal_store_failed", fmt.Sprintf(
			"%s is revoked, so its token no longer works, and its level could not be taken back from %s: %v", revoked.ID, revoked.PeerPrincipalID, err)}
	}
	if !tell {
		return revoked, "the other server removed the connection, so it was not told", nil
	}
	notice := "the other server was told"
	if err := s.callPeerConnection(ctx, connection, http.MethodPost, peerAccessRevokedPath, msg.PeerAccessRevocation{GrantID: revoked.ID, Reason: reason}, http.StatusOK, nil); err != nil {
		notice = fmt.Sprintf("the other server could not be told (%v); its token no longer works here, and it will learn so on its next call", err)
	}
	return revoked, notice, nil
}

// connectionOfConnectionCall says which connection a connection call is on:
// the socket it arrived over, or the credential it presents by https.
func (s *Server) connectionOfConnectionCall(r *http.Request) (store.PeerConnection, *requestCredentialError) {
	if connectionID, overSocket := peerSocketConnectionOfRequest(r); overSocket {
		connection, err := s.store.GetPeerConnection(connectionID)
		if err != nil {
			return store.PeerConnection{}, &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
		}
		if connection.RemovedAt != nil {
			return store.PeerConnection{}, &requestCredentialError{http.StatusUnauthorized, "peer_connection_removed", connection.ID + " was removed"}
		}
		return connection, nil
	}
	if s.cfg.DemoLoginEnabled {
		return store.PeerConnection{}, &requestCredentialError{http.StatusConflict, "demo_login_enabled",
			"a connection call by https is refused while demo login is on: it is a call from another machine, and demo login signs anyone in as anyone"}
	}
	connection, credentialError := s.connectionPresentingCredential(r)
	if credentialError != nil {
		return store.PeerConnection{}, credentialError
	}
	if connection.Transport != msg.PeerTransportDirect {
		return store.PeerConnection{}, &requestCredentialError{http.StatusConflict, "peer_connection_uses_its_socket", connection.ID + " is dial_out, so its connection calls travel over its socket"}
	}
	return connection, nil
}

// connectionPresentingCredential finds the live connection whose credential
// the request presents in X-LLM-Bridge-Peer-Connection.
func (s *Server) connectionPresentingCredential(r *http.Request) (store.PeerConnection, *requestCredentialError) {
	presented := r.Header.Values(peerConnectionCredentialHeader)
	if len(presented) != 1 || strings.TrimSpace(presented[0]) == "" {
		return store.PeerConnection{}, &requestCredentialError{http.StatusUnauthorized, "peer_connection_credential_missing", "send one " + peerConnectionCredentialHeader}
	}
	connection, err := s.store.GetPeerConnectionByInboundCredentialHash(hashPeerSecret(presented[0]))
	if errors.Is(err, store.ErrPeerConnectionNotFound) {
		return store.PeerConnection{}, &requestCredentialError{http.StatusUnauthorized, "peer_connection_credential_unknown", peerConnectionCredentialHeader + " names no connection here"}
	}
	if err != nil {
		return store.PeerConnection{}, &requestCredentialError{http.StatusInternalServerError, "store_failed", err.Error()}
	}
	if connection.RemovedAt != nil {
		return store.PeerConnection{}, &requestCredentialError{http.StatusUnauthorized, "peer_connection_removed", connection.ID + " was removed"}
	}
	s.noteOtherServerIdentity(connection.ID, r.Header)
	return connection, nil
}

// handleReceivePeerAccess: POST /peer/access. The other server delivers a
// token for access on it; this server keeps it in auth-store, ending what the
// connection held before.
func (s *Server) handleReceivePeerAccess(w http.ResponseWriter, r *http.Request) {
	connection, credentialError := s.connectionOfConnectionCall(r)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	var delivery msg.PeerAccessDelivery
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&delivery); err != nil || delivery.GrantID == "" || delivery.PeerPrincipalID == "" || delivery.Level == "" || delivery.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "send grant_id, peer_principal_id, level and token")
		return
	}
	credential, err := s.authClient.Create(r.Context(), authstoreclient.CredentialInput{
		Provider: peerCredentialProvider, Owner: "llm-bridge-server", Account: delivery.PeerPrincipalID, Label: connection.DisplayName,
		IntendedApp: "llm-bridge-server", AuthType: "token", RefreshMode: "none", Token: delivery.Token, BaseURL: connection.RemoteBaseURL,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "auth_store_failed", err.Error())
		return
	}
	access := store.PeerAccess{ConnectionID: connection.ID, RemoteGrantID: delivery.GrantID, RemotePeerPrincipalID: delivery.PeerPrincipalID,
		Level: delivery.Level, CredentialID: credential.ID, ReceivedAt: time.Now().UTC()}
	replaced, err := s.store.InsertPeerAccess(&access)
	if err != nil {
		if deleteErr := s.authClient.Delete(r.Context(), credential.ID); deleteErr != nil {
			log.Printf("[peers] ERROR: delete auth-store credential %s after a failed access record: %v", credential.ID, deleteErr)
		}
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	for _, previous := range replaced {
		if err := s.authClient.Delete(r.Context(), previous.CredentialID); err != nil {
			log.Printf("[peers] ERROR: delete replaced access %s's credential %s: %v", previous.ID, previous.CredentialID, err)
		}
	}
	writeJSONStatus(w, http.StatusCreated, access)
}

// handleReceivePeerAccessRevoked: POST /peer/access/revoked. The other server
// says a grant no longer works; this server ends the access and deletes the
// token.
func (s *Server) handleReceivePeerAccessRevoked(w http.ResponseWriter, r *http.Request) {
	connection, credentialError := s.connectionOfConnectionCall(r)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	var revocation msg.PeerAccessRevocation
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&revocation); err != nil || revocation.GrantID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "send grant_id and reason")
		return
	}
	ended, err := s.store.EndPeerAccess(connection.ID, revocation.GrantID, "revoked by the other server: "+revocation.Reason, time.Now().UTC())
	if errors.Is(err, store.ErrPeerAccessNotFound) {
		writeJSONError(w, http.StatusNotFound, "peer_access_not_found", err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if err := s.authClient.Delete(r.Context(), ended.CredentialID); err != nil {
		log.Printf("[peers] ERROR: delete revoked access %s's credential %s: %v", ended.ID, ended.CredentialID, err)
	}
	writeJSON(w, ended)
}

// handlePeerSelf: GET /peer/self. Tells a peer who its token acts as here.
func (s *Server) handlePeerSelf(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerOfRequest(r)
	if !ok || caller.peer == nil {
		writeJSONError(w, http.StatusBadRequest, "not_a_peer", fmt.Sprintf("GET %s answers a caller presenting %s", peerSelfPath, peerTokenHeader))
		return
	}
	writeJSON(w, msg.PeerGrantSelf{GrantID: caller.peer.grantID, ConnectionID: caller.peer.connectionID, PeerPrincipalID: caller.principalID,
		Level: caller.peer.level, IsAdministrator: caller.isAdministrator})
}

// handleCheckPeerConnection: POST /peer-connections/{id}/check. Asks the
// other server who the access this server holds acts as there.
func (s *Server) handleCheckPeerConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := s.livePeerConnectionOf(w, r)
	if !ok {
		return
	}
	response, err := s.callWithHeldAccess(r.Context(), connection, http.MethodGet, peerSelfPath, http.Header{}, nil)
	if err != nil {
		writePeerCallError(w, err)
		return
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
}

// handleRemovePeerConnection: POST /peer-connections/{id}/remove. Revokes the
// access this server gives, ends the access it holds, closes the socket and
// deletes the connection's credential.
func (s *Server) handleRemovePeerConnection(w http.ResponseWriter, r *http.Request) {
	administratorID, ok := administratorMakingPeerChange(w, r)
	if !ok {
		return
	}
	var request msg.PeerReasonRequest
	if !decodeAdministrationBody(w, r, &request, "removed_by") {
		return
	}
	if strings.TrimSpace(request.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "reason_required", "say why; the reason stays on the row")
		return
	}
	connection, ok := s.livePeerConnectionOf(w, r)
	if !ok {
		return
	}
	if _, _, failure := s.revokeLivePeerGrant(r.Context(), connection, administratorID, "connection removed: "+request.Reason); failure != nil && failure.code != "peer_grant_not_found" {
		failure.write(w)
		return
	}
	if ended, err := s.store.EndPeerAccess(connection.ID, "", "connection removed: "+request.Reason, time.Now().UTC()); err == nil {
		if err := s.authClient.Delete(r.Context(), ended.CredentialID); err != nil {
			log.Printf("[peers] ERROR: delete ended access %s's credential %s: %v", ended.ID, ended.CredentialID, err)
		}
	} else if !errors.Is(err, store.ErrPeerAccessNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	// Told before the socket closes and the credential goes, which are what
	// the call travels by. Best effort: the connection is removed here
	// whether or not the other server hears, and the row says which.
	removalNotice := "the other server was told"
	if err := s.callPeerConnection(r.Context(), connection, http.MethodPost, peerConnectionRemovedPath, msg.PeerConnectionRemovedNotice{Reason: request.Reason}, http.StatusOK, nil); err != nil {
		removalNotice = fmt.Sprintf("the other server could not be told (%v); its calls are refused from now on", err)
		log.Printf("[peers] %s removed: %s", connection.ID, removalNotice)
	}
	s.peerSockets.stop(connection.ID)
	removed, err := s.store.RemovePeerConnection(connection.ID, administratorID, request.Reason, time.Now().UTC())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if err := s.store.RecordPeerConnectionRemovalNotice(connection.ID, removalNotice); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	removed.RemovalNotice = removalNotice
	if err := s.authClient.Delete(r.Context(), connection.OutboundCredentialID); err != nil {
		writeJSONError(w, http.StatusBadGateway, "auth_store_failed", fmt.Sprintf("%s is removed, and auth-store credential %s could not be deleted: %v", connection.ID, connection.OutboundCredentialID, err))
		return
	}
	writeJSON(w, s.peerConnectionView(removed))
}

// handleReceivePeerConnectionRemoved: POST /peer/connection/removed. The
// other server removed the connection and says so over it. This server
// revokes the access it gave, ends the access it held, closes the socket and
// deletes the connection's credential, as its own remove would, and marks the
// connection removed by the other server.
func (s *Server) handleReceivePeerConnectionRemoved(w http.ResponseWriter, r *http.Request) {
	connection, credentialError := s.connectionOfConnectionCall(r)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	var notice msg.PeerConnectionRemovedNotice
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&notice); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "send reason")
		return
	}
	reason := "removed by the other server: " + notice.Reason
	if _, _, failure := s.revokeLivePeerGrantTelling(r.Context(), connection, "", reason, false); failure != nil && failure.code != "peer_grant_not_found" {
		failure.write(w)
		return
	}
	if ended, err := s.store.EndPeerAccess(connection.ID, "", reason, time.Now().UTC()); err == nil {
		if err := s.authClient.Delete(r.Context(), ended.CredentialID); err != nil {
			log.Printf("[peers] ERROR: delete ended access %s's credential %s: %v", ended.ID, ended.CredentialID, err)
		}
	} else if !errors.Is(err, store.ErrPeerAccessNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	removed, err := s.store.RemovePeerConnectionByOtherServer(connection.ID, notice.Reason, time.Now().UTC())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if err := s.authClient.Delete(r.Context(), connection.OutboundCredentialID); err != nil {
		log.Printf("[peers] ERROR: %s was removed by the other server, and auth-store credential %s could not be deleted: %v", connection.ID, connection.OutboundCredentialID, err)
	}
	// Answered before the socket closes: the answer travels over it. The
	// dialer must not reopen it, so it is stopped once this answer is out.
	writeJSON(w, s.peerConnectionView(removed))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	closeAfter := peerSocketCloseAfterRemovalNotice
	go func() {
		time.Sleep(closeAfter)
		s.peerSockets.stop(connection.ID)
	}()
}

// peerSocketCloseAfterRemovalNotice is how long a server that heard the
// connection was removed keeps the socket, so its answer reaches the other
// side first.
var peerSocketCloseAfterRemovalNotice = 2 * time.Second

// errPeerAccessNotHeld is a call that needs access the other server never
// gave, or took back.
var errPeerAccessNotHeld = errors.New("the other server has given this server no access")

// writePeerCallError answers a failed call to the other server.
func writePeerCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPeerAccessNotHeld):
		writeJSONError(w, http.StatusConflict, "peer_access_not_held", err.Error())
	case errors.Is(err, errPeerNotConnected):
		writeJSONError(w, http.StatusServiceUnavailable, "peer_not_connected", err.Error())
	case errors.As(err, new(*errPeerNotAnswering)):
		writeJSONError(w, http.StatusServiceUnavailable, "peer_not_answering", err.Error())
	default:
		writeJSONError(w, http.StatusBadGateway, "peer_unreachable", err.Error())
	}
}

// outboundConnectionCredential reads the credential this server presents on
// the connection's calls from auth-store.
func (s *Server) outboundConnectionCredential(ctx context.Context, connection store.PeerConnection) (string, error) {
	resolved, err := s.authClient.Resolve(ctx, connection.OutboundCredentialID, "peer connection "+connection.ID+" calls "+connection.DisplayName)
	if err != nil {
		return "", fmt.Errorf("read %s's credential from auth-store credential %s: %w", connection.ID, connection.OutboundCredentialID, err)
	}
	if resolved.Secret() == "" {
		return "", fmt.Errorf("auth-store credential %s holds nothing for %s", connection.OutboundCredentialID, connection.ID)
	}
	return resolved.Secret(), nil
}

// sendOverPeerConnection sends one request to the other server: by https on
// a direct connection, over the socket on a dial_out one. setHeaders adds
// credentials; on a socket the connection itself is the credential for a
// connection call, so a direct-only header is added only for https.
func (s *Server) sendOverPeerConnection(ctx context.Context, client *http.Client, connection store.PeerConnection, method, path string, header http.Header, body io.Reader, contentLength int64, connectionCall bool) (*http.Response, error) {
	header = header.Clone()
	if header == nil {
		header = http.Header{}
	}
	s.stampThisServersIdentity(header)
	if connection.Transport == msg.PeerTransportDialOut {
		socket := s.peerSockets.socketOf(connection.ID)
		if socket == nil {
			return nil, errPeerNotConnected
		}
		if err := socket.notAnswering(); err != nil {
			return nil, err
		}
		response, err := socket.roundTrip(ctx, method, path, header, body)
		if err == nil {
			s.noteOtherServerIdentity(connection.ID, response.Header)
		}
		return response, err
	}
	if connection.RemoteBaseURL == "" {
		return nil, fmt.Errorf("%s is direct and has no address for the other server", connection.ID)
	}
	request, err := http.NewRequestWithContext(ctx, method, connection.RemoteBaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s%s: %w", method, connection.RemoteBaseURL, path, err)
	}
	if body != nil {
		request.ContentLength = contentLength
	}
	for key, values := range header {
		request.Header[key] = values
	}
	if connectionCall {
		credential, err := s.outboundConnectionCredential(ctx, connection)
		if err != nil {
			return nil, err
		}
		request.Header.Set(peerConnectionCredentialHeader, credential)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s%s: %w", method, connection.RemoteBaseURL, path, err)
	}
	s.noteOtherServerIdentity(connection.ID, response.Header)
	return response, nil
}

// callPeerConnection makes one connection call with a JSON body and reads a
// JSON answer of the wanted status into answer when it is not nil.
func (s *Server) callPeerConnection(ctx context.Context, connection store.PeerConnection, method, path string, body any, wantStatus int, answer any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	header := http.Header{"Content-Type": {"application/json"}}
	response, err := s.sendOverPeerConnection(ctx, peerHTTPClient, connection, method, path, header, bytes.NewReader(data), int64(len(data)), true)
	if err != nil {
		return err
	}
	return readPeerAnswer(response, method+" "+path, wantStatus, answer)
}

// callWithHeldAccess sends one request to the other server presenting the
// peer token it gave this server.
func (s *Server) callWithHeldAccess(ctx context.Context, connection store.PeerConnection, method, path string, header http.Header, body io.Reader) (*http.Response, error) {
	return s.callWithHeldAccessThrough(ctx, peerHTTPClient, connection, method, path, header, body, -1)
}

func (s *Server) callWithHeldAccessThrough(ctx context.Context, client *http.Client, connection store.PeerConnection, method, path string, header http.Header, body io.Reader, contentLength int64) (*http.Response, error) {
	access, err := s.store.LivePeerAccessOfConnection(connection.ID)
	if errors.Is(err, store.ErrPeerAccessNotFound) {
		return nil, fmt.Errorf("%w on %s", errPeerAccessNotHeld, connection.ID)
	}
	if err != nil {
		return nil, err
	}
	resolved, err := s.authClient.Resolve(ctx, access.CredentialID, "peer access "+access.ID+" calls "+connection.DisplayName)
	if err != nil {
		return nil, fmt.Errorf("read %s's token from auth-store credential %s: %w", access.ID, access.CredentialID, err)
	}
	if resolved.Secret() == "" {
		return nil, fmt.Errorf("auth-store credential %s holds no token for %s", access.CredentialID, access.ID)
	}
	header = header.Clone()
	if header == nil {
		header = http.Header{}
	}
	header.Set(peerTokenHeader, resolved.Secret())
	return s.sendOverPeerConnection(ctx, client, connection, method, path, header, body, contentLength, false)
}

// postJSONToPeer posts a JSON body to another server's address before any
// connection exists, and reads a JSON answer of the wanted status.
func postJSONToPeer(ctx context.Context, target string, header http.Header, body any, wantStatus int, answer any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build POST %s: %w", target, err)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := peerHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("POST %s: %w", target, err)
	}
	return readPeerAnswer(response, "POST "+target, wantStatus, answer)
}

func readPeerAnswer(response *http.Response, what string, wantStatus int, answer any) error {
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read the answer to %s: %w", what, err)
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%s answered %d: %s", what, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if answer != nil {
		if err := json.Unmarshal(data, answer); err != nil {
			return fmt.Errorf("%s answered %d with a body this server cannot read: %w", what, response.StatusCode, err)
		}
	}
	return nil
}

// writeJSONStatus writes payload as JSON with the given status.
func writeJSONStatus(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}
