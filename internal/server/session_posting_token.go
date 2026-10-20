package server

// A session posting token lets a session's agent post into its own session —
// share a file, or offer an action for a person to confirm — and do nothing
// else. It cannot run an action: confirming one takes the session's owner.
//
// Every local harness child gets one in LLM_BRIDGE_SESSION_POSTING_TOKEN, with
// the addresses to post to in LLM_BRIDGE_SESSION_FILES_URL and
// LLM_BRIDGE_SESSION_ACTIONS_URL. It is not the session agent token
// (session_agent_token.go): that one acts as the session's principal at the
// store proxies, so it is minted only for a session started as a principal —
// and on this host no session is (2026-09-27: 0 of 1,098 in a week). An agent
// showing the user an image needs no principal. It needs to prove which
// session it is, and this token proves exactly that.
//
// It is accepted on one route class, routeSessionOwnerOrItsAgentPostsIntoSession,
// and only for the session it names. It is signed with the demo login signing
// key under its own domain separator, so it can never verify as a login cookie
// or a session agent token, nor they as it. It carries no expiry: it is worth
// only "put something into this session", it is re-minted at every spawn, and
// a session that no longer exists refuses it. Until 2026-09-28 it was the
// session file token, in LLM_BRIDGE_SESSION_FILE_TOKEN, and could only share
// files.

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
	"github.com/kayushkin/llm-bridge/msg"
)

// Names of the variables every local harness child receives for posting into
// its session.
const (
	sessionPostingTokenEnvironmentVariable = "LLM_BRIDGE_SESSION_POSTING_TOKEN"
	sessionFilesURLEnvironmentVariable     = "LLM_BRIDGE_SESSION_FILES_URL"
	sessionActionsURLEnvironmentVariable   = "LLM_BRIDGE_SESSION_ACTIONS_URL"
	sessionImagesURLEnvironmentVariable    = "LLM_BRIDGE_SESSION_IMAGES_URL"
)

const sessionPostingTokenPrefix = "llm-bridge-session-posting-token-v1"

const sessionPostingTokenSignatureDomain = "llm-bridge-server session posting token v1\x00"

// encodeSessionPostingToken signs a token naming sessionID.
//
// Shape: <prefix>.<base64url session id>.<signature>. The session id is
// base64url-encoded because a caller-minted session id may contain dots.
func (codec *harnessChildTokenSigner) encodeSessionPostingToken(sessionID string) string {
	signedPortion := sessionPostingTokenPrefix + "." + base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	return signedPortion + "." + codec.sessionPostingTokenSignature(signedPortion)
}

func (codec *harnessChildTokenSigner) sessionPostingTokenSignature(signedPortion string) string {
	mac := hmac.New(sha256.New, codec.signingKey)
	mac.Write([]byte(sessionPostingTokenSignatureDomain))
	mac.Write([]byte(signedPortion))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeSessionPostingToken verifies a token and returns the session it names.
func (codec *harnessChildTokenSigner) decodeSessionPostingToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != sessionPostingTokenPrefix {
		return "", errors.New("not a session posting token")
	}
	signedPortion := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(codec.sessionPostingTokenSignature(signedPortion))) {
		return "", errors.New("session posting token signature does not verify")
	}
	sessionID, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(sessionID) == 0 {
		return "", errors.New("session posting token names a malformed session id")
	}
	return string(sessionID), nil
}

// sessionPostingEnvironmentForInstance is sessionPostingEnvironment for a spawn
// on inst. A local child receives environment variables directly and a runner
// child through RunnerSpawn.Env; the ssh transport has no way to deliver them,
// and StartOnInstance refuses a session that carries any there. Posting is an
// extra, so on ssh the session starts without it and the log says so. The
// addresses are this server's, which a runner reaches through its tunnel.
func (s *Server) sessionPostingEnvironmentForInstance(session *store.Session, inst *msg.Instance) []string {
	if inst.Machine == nil || inst.Machine.Transport == msg.TransportSSH {
		transport := msg.Transport("unknown")
		if inst.Machine != nil {
			transport = inst.Machine.Transport
		}
		log.Printf("[session-posting] %s: instance %s uses the %s transport, which cannot deliver environment variables; the agent cannot share files or offer actions", session.SessionID, inst.ID, transport)
		return nil
	}
	return s.sessionPostingEnvironment(session)
}

// sessionPostingEnvironment returns the variables that let a session's agent
// post into it. Nothing when this server's own address is unknown, and no
// files address when file-store is not wired: posting is an extra, and a spawn
// must not fail for want of it.
func (s *Server) sessionPostingEnvironment(session *store.Session) []string {
	gatewayURL := s.gatewayURLForSessionAgents()
	if gatewayURL == "" {
		log.Printf("[session-posting] %s: neither LLMBRIDGE_PUBLIC_URL nor LLMBRIDGE_LISTEN_ADDR gives this server's address; the agent cannot share files or offer actions", session.SessionID)
		return nil
	}
	sessionURL := fmt.Sprintf("%s/sessions/%s", gatewayURL, url.PathEscape(session.SessionID))
	environment := []string{
		sessionPostingTokenEnvironmentVariable + "=" + s.harnessChildTokenSigner.encodeSessionPostingToken(session.SessionID),
		sessionActionsURLEnvironmentVariable + "=" + sessionURL + "/actions",
	}
	if s.fileStore != nil {
		environment = append(environment,
			sessionFilesURLEnvironmentVariable+"="+sessionURL+"/files",
			sessionImagesURLEnvironmentVariable+"="+sessionURL+"/images")
	}
	return environment
}
