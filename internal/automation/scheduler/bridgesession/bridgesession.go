// Package bridgesession builds the llm-bridge-server URLs that address one
// session by id.
//
// Every binary in this repo that asks the bridge about a session takes that id
// from somewhere it does not control — a kanban card_link's entity_ref, a
// noteboard body, a NATS message. Interpolating such an id straight into a path
// with fmt.Sprintf lets it address a different resource, and the caller cannot
// tell that happened, because the reply to the wrong URL is a well-formed reply.
//
// Measured against the llm-bridge-server running on this host, 2026-08-14, with
// a real session id standing in for {id}:
//
//	/sessions/{id}?x=1           200 — and it is a DIFFERENT session's state,
//	                             because the query separator cuts the id short
//	/sessions/{id}/send          405 — a sibling route, reached by a ref with a slash
//	/sessions/{id}/messages      200 — a JSON array, which decodes into nothing
//	/sessions/{id}/attach-token  404 — for a session that exists and may be running
//	/sessions/..                 307 -> "/" -> 404, and Go's http.Client follows it
//	/sessions/.                  307 -> "/sessions" -> 200, the whole session list
//
// The 404s are the dangerous ones. email-classifier's session gate reads 404 as
// "the bridge has forgotten this session, so the card is free" — the one reply
// that lets it edit a card an agent is still holding. Its own comment says why
// the distinction has to survive (cmd/email-classifier/session.go). So an id
// that goes astray does not fail loudly here; it fails open.
package bridgesession

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// URL returns the llm-bridge-server URL addressing the session itself:
// GET /sessions/{id}.
func URL(bridgeBaseURL, sessionID string) (string, error) {
	segment, err := sessionIDAsPathSegment(sessionID)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(bridgeBaseURL, "/") + "/sessions/" + segment, nil
}

// SubresourceURL returns the URL addressing one sub-resource of a session:
// /sessions/{id}/messages, /sessions/{id}/send.
//
// subresource is a route this repo names as a literal at every call site, so it
// is joined verbatim. Only sessionID arrives from outside, and only sessionID
// is escaped.
func SubresourceURL(bridgeBaseURL, sessionID, subresource string) (string, error) {
	sessionURL, err := URL(bridgeBaseURL, sessionID)
	if err != nil {
		return "", err
	}
	return sessionURL + "/" + strings.Trim(subresource, "/"), nil
}

// sessionIDAsPathSegment percent-escapes sessionID into exactly one path
// segment, or reports why no escaping can make it one.
//
// url.PathEscape is necessary and not sufficient, and the gap is small enough
// to state exactly rather than guess at. Of the 128 ASCII bytes it leaves 72
// unescaped; every one of those is an ordinary path character except ".", and
// it never emits "/", so an escaped id can never split into two segments. What
// it does not do is escape a dot-segment: PathEscape("..") is "..", which a
// router resolves before matching any route at all.
//
// A dot-segment therefore cannot be made to name a session, and neither can the
// empty string, which addresses the collection. Both are refused. Refusing is
// the same answer these callers already give an unreachable bridge — an error —
// and every one of them treats an error as "I could not tell", which is the
// fail-safe side.
func sessionIDAsPathSegment(sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("empty session id addresses the session collection, not a session")
	}
	if sessionID == "." || sessionID == ".." {
		return "", fmt.Errorf("session id %q is a dot-segment a router resolves away before matching a route", sessionID)
	}
	return url.PathEscape(sessionID), nil
}
