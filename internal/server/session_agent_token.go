package server

// Session agent tokens: how an agent's tool calls act as the session's
// principal.
//
// When a session started as a principal is spawned, its harness child is
// given LLM_BRIDGE_GATEWAY_URL and
// LLM_BRIDGE_PRINCIPAL_TOKEN. An MCP server or a shell command the agent runs
// sends the token as "Authorization: Bearer <token>" to the gateway's
// identity-carrying store proxies (/kanban/ and /grant-store/), and the
// request reaches the store as the session's principal, exactly as a login
// cookie would. The token is accepted nowhere else.
//
// The token is signed with the demo login signing key under its own domain
// separator, so it can never verify as a login cookie and a cookie can never
// verify as a token. It carries the principal, the session and an expiry, and
// every request that presents it is checked against the store: the session
// must still exist, still belong to that principal, and not be in a terminal
// state. That is how a token dies with its session.
//
// This binds only agents that go through the gateway. In deployment
// kanban-store and grant-store must not be reachable from agent processes,
// and no store service token may be in an agent's environment
// (internal/childprocessenv removes the ones this server knows).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Names of the variables a principal's harness child receives.
const (
	gatewayURLEnvironmentVariable     = "LLM_BRIDGE_GATEWAY_URL"
	principalTokenEnvironmentVariable = "LLM_BRIDGE_PRINCIPAL_TOKEN"
)

// sessionAgentTokenPrefix starts every session agent token, so a token is
// recognisable in a log and never has the three-part shape of a login cookie.
const sessionAgentTokenPrefix = "llm-bridge-session-agent-token-v1"

// sessionAgentTokenSignatureDomain is prepended to the signed bytes of a
// session agent token and of nothing else. A login cookie signs
// "principal_….<expiry>", which cannot begin with this, so no signature is
// valid for both.
const sessionAgentTokenSignatureDomain = "llm-bridge-server session agent token v1\x00"

// sessionAgentTokenLifetime bounds a token independently of its session. A
// token is minted at every spawn and resume, so a session that runs longer
// than this without being respawned loses gateway access until it is.
const sessionAgentTokenLifetime = 24 * time.Hour

// sessionStatesThatEndSessionAgentTokens are the terminal session states: a
// session in one of them has no running agent that should still act.
var sessionStatesThatEndSessionAgentTokens = []msg.SessionState{
	msg.SessionCompleted,
	msg.SessionError,
	msg.SessionAborted,
	msg.SessionDisconnected,
}

// sessionAgentTokenClaims is what a verified token says.
type sessionAgentTokenClaims struct {
	PrincipalID string
	SessionID   string
	ExpiresAt   time.Time
}

// encodeSessionAgentToken signs claims with the demo login signing key.
//
// Shape: <prefix>.<principal id>.<base64url session id>.<expiry seconds>.<signature>.
// The session id is base64url-encoded because a caller-minted session id may
// contain dots.
func (codec *principalSessionCookieCodec) encodeSessionAgentToken(claims sessionAgentTokenClaims) string {
	signedPortion := sessionAgentTokenPrefix + "." + claims.PrincipalID + "." +
		base64.RawURLEncoding.EncodeToString([]byte(claims.SessionID)) + "." +
		strconv.FormatInt(claims.ExpiresAt.Unix(), 10)
	return signedPortion + "." + codec.sessionAgentTokenSignature(signedPortion)
}

func (codec *principalSessionCookieCodec) sessionAgentTokenSignature(signedPortion string) string {
	mac := hmac.New(sha256.New, codec.signingKey)
	mac.Write([]byte(sessionAgentTokenSignatureDomain))
	mac.Write([]byte(signedPortion))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeSessionAgentToken verifies a token's shape, signature and expiry.
// It does not look at the session; principalOfSessionAgentToken does.
func (codec *principalSessionCookieCodec) decodeSessionAgentToken(token string) (sessionAgentTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != sessionAgentTokenPrefix {
		return sessionAgentTokenClaims{}, errors.New("not a session agent token")
	}
	signedPortion := strings.Join(parts[:4], ".")
	if !hmac.Equal([]byte(parts[4]), []byte(codec.sessionAgentTokenSignature(signedPortion))) {
		return sessionAgentTokenClaims{}, errors.New("session agent token signature does not verify")
	}
	if !isWellFormedPrincipalID(parts[1]) {
		return sessionAgentTokenClaims{}, errors.New("session agent token names a malformed principal id")
	}
	sessionID, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sessionID) == 0 {
		return sessionAgentTokenClaims{}, errors.New("session agent token names a malformed session id")
	}
	expirySeconds, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return sessionAgentTokenClaims{}, errors.New("session agent token expiry is not a number")
	}
	expiresAt := time.Unix(expirySeconds, 0).UTC()
	if !codec.now().Before(expiresAt) {
		return sessionAgentTokenClaims{}, errors.New("session agent token has expired")
	}
	return sessionAgentTokenClaims{PrincipalID: parts[1], SessionID: string(sessionID), ExpiresAt: expiresAt}, nil
}

// principalOfSessionAgentToken verifies a presented token and the session it
// names, and returns the principal the request acts as.
func (s *Server) principalOfSessionAgentToken(token string) (string, error) {
	claims, err := s.principalSessionCookieCodec.decodeSessionAgentToken(token)
	if err != nil {
		return "", err
	}
	session, err := s.store.GetSession(claims.SessionID)
	if err != nil {
		return "", fmt.Errorf("session agent token names session %s, which does not exist", claims.SessionID)
	}
	if session.PrincipalID != claims.PrincipalID {
		return "", fmt.Errorf("session agent token names principal %s, and session %s is not started as that principal", claims.PrincipalID, claims.SessionID)
	}
	for _, terminalState := range sessionStatesThatEndSessionAgentTokens {
		if msg.SessionState(session.State) == terminalState {
			return "", fmt.Errorf("session %s has ended (state %s), and its session agent token ended with it", claims.SessionID, session.State)
		}
	}
	return claims.PrincipalID, nil
}

// sessionAgentEnvironment returns the variables a session's harness child
// receives so its tool calls can act as the session's principal: none unless
// the session is started as a principal. A gateway
// URL that cannot be determined is an error — a child given a token and no
// place to send it would fail at its first tool call instead of at spawn.
func (s *Server) sessionAgentEnvironment(session *store.Session) ([]string, error) {
	if session.PrincipalID == "" {
		return nil, nil
	}
	gatewayURL := s.gatewayURLForSessionAgents()
	if gatewayURL == "" {
		return nil, fmt.Errorf("session %s acts for %s and needs %s, and neither LLMBRIDGE_PUBLIC_URL nor LLMBRIDGE_LISTEN_ADDR gives this server's address",
			session.SessionID, session.PrincipalID, gatewayURLEnvironmentVariable)
	}
	token := s.principalSessionCookieCodec.encodeSessionAgentToken(sessionAgentTokenClaims{
		PrincipalID: session.PrincipalID,
		SessionID:   session.SessionID,
		ExpiresAt:   s.principalSessionCookieCodec.now().Add(sessionAgentTokenLifetime).UTC().Truncate(time.Second),
	})
	return []string{
		gatewayURLEnvironmentVariable + "=" + gatewayURL,
		principalTokenEnvironmentVariable + "=" + token,
	}, nil
}

// gatewayURLForSessionAgents is the base URL a harness child reaches this
// server at: LLMBRIDGE_PUBLIC_URL when set, otherwise the address it listens
// on, the same one the permission prehook URLs are built from.
func (s *Server) gatewayURLForSessionAgents() string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	if s.cfg.ListenAddr == "" {
		return ""
	}
	return publicBaseURL(s.cfg.ListenAddr)
}
