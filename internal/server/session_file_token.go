package server

// A session file token lets a session's agent share a file into its own
// session, and do nothing else.
//
// Every local harness child gets one, with the upload address, in
// LLM_BRIDGE_SESSION_FILE_TOKEN and LLM_BRIDGE_SESSION_FILES_URL. It is not the
// session agent token (session_agent_token.go): that one acts as the session's
// principal at the store proxies, so it is minted only for a session started as
// a principal — and on this host no session is (2026-09-27: 0 of 1,098 in a
// week). An agent showing the user an image needs no principal. It needs to
// prove which session it is, and this token proves exactly that.
//
// It is accepted on one route class, routeSessionOwnerOrItsAgentSharesFile, and
// only for the session it names. It is signed with the demo login signing key
// under its own domain separator, so it can never verify as a login cookie or a
// session agent token, nor they as it. It carries no expiry: it is worth only
// "put a file into this session", it is re-minted at every spawn, and a session
// that no longer exists refuses it.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Names of the variables every local harness child receives for sharing files.
const (
	sessionFileTokenEnvironmentVariable = "LLM_BRIDGE_SESSION_FILE_TOKEN"
	sessionFilesURLEnvironmentVariable  = "LLM_BRIDGE_SESSION_FILES_URL"
)

const sessionFileTokenPrefix = "llm-bridge-session-file-token-v1"

const sessionFileTokenSignatureDomain = "llm-bridge-server session file token v1\x00"

// encodeSessionFileToken signs a token naming sessionID.
//
// Shape: <prefix>.<base64url session id>.<signature>. The session id is
// base64url-encoded because a caller-minted session id may contain dots.
func (codec *principalSessionCookieCodec) encodeSessionFileToken(sessionID string) string {
	signedPortion := sessionFileTokenPrefix + "." + base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	return signedPortion + "." + codec.sessionFileTokenSignature(signedPortion)
}

func (codec *principalSessionCookieCodec) sessionFileTokenSignature(signedPortion string) string {
	mac := hmac.New(sha256.New, codec.signingKey)
	mac.Write([]byte(sessionFileTokenSignatureDomain))
	mac.Write([]byte(signedPortion))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeSessionFileToken verifies a token and returns the session it names.
func (codec *principalSessionCookieCodec) decodeSessionFileToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != sessionFileTokenPrefix {
		return "", errors.New("not a session file token")
	}
	signedPortion := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(codec.sessionFileTokenSignature(signedPortion))) {
		return "", errors.New("session file token signature does not verify")
	}
	sessionID, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(sessionID) == 0 {
		return "", errors.New("session file token names a malformed session id")
	}
	return string(sessionID), nil
}

// sessionFileSharingEnvironment returns the variables that let a session's
// agent share files into it. Nothing when file-store is not wired, or when this
// server's own address is unknown: sharing is an extra, and a spawn must not
// fail for want of it.
func (s *Server) sessionFileSharingEnvironment(session *store.Session) []string {
	if s.fileStore == nil {
		return nil
	}
	gatewayURL := s.gatewayURLForSessionAgents()
	if gatewayURL == "" {
		log.Printf("[session-files] %s: neither LLMBRIDGE_PUBLIC_URL nor LLMBRIDGE_LISTEN_ADDR gives this server's address; the agent cannot share files", session.SessionID)
		return nil
	}
	return []string{
		sessionFileTokenEnvironmentVariable + "=" + s.principalSessionCookieCodec.encodeSessionFileToken(session.SessionID),
		sessionFilesURLEnvironmentVariable + "=" + fmt.Sprintf("%s/sessions/%s/files", gatewayURL, url.PathEscape(session.SessionID)),
	}
}
