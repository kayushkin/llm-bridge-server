package server

import (
	"net/url"
	"path"
	"strings"
)

// escapedPathAfterPrefix returns the part of u's escaped path that follows
// mountPrefix, keeping every percent-escape the caller sent.
//
// The path is read with EscapedPath rather than Path, and that is the whole
// point of this function. r.URL.Path is percent-DECODED, so pasting it into a
// new URL string hands the separators back their meaning: %2F re-reads as a
// segment separator, %3F as the start of the query and %23 as the start of the
// fragment. The last two truncate the path before the request is ever sent.
//
// Measured against a stand-in upstream, forwarding r.URL.Path:
//
//	caller sent /api/skill-store/skills/a%2Fb/files
//	upstream got /skills/a/b/files           — one segment became two
//	caller sent /api/skill-store/skills/a%3Fx=1/files
//	upstream got /skills/a?x=1/files         — path truncated, query injected
//	caller sent /api/skill-store/skills/a%23frag/files
//	upstream got /skills/a                   — rest of the path silently dropped
//
// A space is not a witness for any of this: Go re-escapes %20 when it parses
// the target back, so a space fixture stays green with the bug present.
//
// The mount prefix is matched a segment at a time against the DECODED form of
// each segment, because that is what routed the request here in the first
// place. It returns the whole escaped path unchanged when the prefix does not
// match, which makes a mismatch an upstream 404 rather than a silently
// different resource.
//
// The same repair, and the measurement above, are in dash's
// server/proxy_target.go and llmux's server/proxy_target.go.
func escapedPathAfterPrefix(u *url.URL, mountPrefix string) string {
	escaped := u.EscapedPath()
	if mountPrefix == "" || mountPrefix == "/" {
		return escaped
	}

	escapedSegments := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	prefixSegments := strings.Split(strings.TrimPrefix(strings.TrimSuffix(mountPrefix, "/"), "/"), "/")
	if len(escapedSegments) < len(prefixSegments) {
		return escaped
	}
	for i, want := range prefixSegments {
		got, err := url.PathUnescape(escapedSegments[i])
		if err != nil || got != want {
			return escaped
		}
	}

	remainder := escapedSegments[len(prefixSegments):]
	if len(remainder) == 0 {
		return ""
	}
	return "/" + strings.Join(remainder, "/")
}

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

// cleanEscapedPathAfterPrefix is escapedPathAfterPrefix with path.Clean applied,
// for the store proxies, which normalise the remainder before forwarding it.
//
// Cleaning the ESCAPED form is what makes the normalisation mean what it says.
// path.Clean resolves ".." against "/" separators, so running it on the decoded
// path let an escaped %2F..%2F become a real traversal that Clean then resolved
// — the caller's single segment silently turned into a walk up the upstream's
// path. On the escaped form a %2F is an ordinary character and stays inside its
// segment, so Clean only removes the dot segments the caller actually sent.
func cleanEscapedPathAfterPrefix(u *url.URL, mountPrefix string) string {
	return path.Clean("/" + strings.TrimPrefix(escapedPathAfterPrefix(u, mountPrefix), "/"))
}
