package httproutes

import (
	"net/url"
	"strings"
)

// EscapedPathAfterPrefix returns the part of u's escaped path that follows
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
// server/proxy_target.go.
func EscapedPathAfterPrefix(u *url.URL, mountPrefix string) string {
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
