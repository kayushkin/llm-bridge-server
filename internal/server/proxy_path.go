package server

import (
	"net/url"
	"strings"
)

// logStoreSessionURL builds the log-store URL for one session's messages or
// history. sessionID is the REAL, decoded id — the same string the in-process
// lookups use — and this is the one place it becomes part of a URL, so this is
// the one place it is escaped.
//
// A session id is a single path SEGMENT. r.PathValue hands it back decoded, so
// an id holding a %2F, %3F or %23 arrives with that character bare; pasted into
// a URL it gets its meaning back as a separator, a query or a fragment, and
// addresses a different resource. Escaping at the point of USE rather than the
// point of READ is what lets both consumers be right at once: the manager
// indexes by the decoded id, the wire needs the escaped one.
// logStoreEndpointFromPath returns the part of a /sessions/{id}/... route that names
// the log-store endpoint — "messages", "history", "messages/raw" — or "" if the path
// is not one.
//
// It replaced a `path.Base` on the same input, which read the LAST segment on the
// stated assumption that the endpoint literal is always last. That held for exactly as
// long as every proxied route was three segments. The first two-segment endpoint,
// `/messages/raw`, proxied to `/api/v1/sessions/{id}/raw` and 404ed — the route was
// registered, the binary had it, and the request still could not arrive.
//
// Splits the ESCAPED path, so a session id containing an encoded slash stays one
// segment and cannot shift the split.
func logStoreEndpointFromPath(escapedPath string) string {
	// ["sessions", "<id>", "messages", ...]
	segments := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(segments) < 3 || segments[0] != "sessions" {
		return ""
	}
	return strings.Join(segments[2:], "/")
}

func logStoreSessionURL(base, sessionID, endpoint string) string {
	return base + "/api/v1/sessions/" + url.PathEscape(sessionID) + "/" + endpoint
}
