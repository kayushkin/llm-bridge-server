package server

import "github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

// The identity-carrying store routes.
//
// kanban-store, grant-store, mailstack and reference-store narrow what a
// caller may see and do by who the caller is. Each runs in this process and
// reads its caller from the request context, set from the principal request
// authorization established (a login cookie, a session agent token, or an
// X-Principal-Id the internal service asserted with its service token), never
// from anything the client sent unchecked. An administrator is passed as
// itself: these stores make their own administrator check. Every identity
// header and credential the client sent is deleted before a store sees the
// request.

const (
	// issuesMountPrefix is where kanban-store is mounted, with the remainder
	// answered at its root, unchanged: /issues/boards reaches /boards.
	// kanban-store runs in this process (kanban_store_routes.go), so it is
	// not proxied: the caller reaches it through the request context, not a
	// header.
	issuesMountPrefix = "/issues"
	// grantsMountPrefix is where grant-store's routes are mounted.
	// grant-store runs in this process (access_routes.go), so they are not
	// proxied: the caller reaches it through the request context, not a header.
	grantsMountPrefix = "/grants"
	// mailMountPrefix is where mailstack's routes are mounted: the
	// remainder of the path is answered at mailstack's root, where /api/…
	// lives. mailstack runs in this process (mailstack_routes.go), so it is
	// not proxied: the caller reaches it through the request context.
	mailMountPrefix = "/mail"
)

// requestHeadersNeverForwardedToStores are deleted from every request a store
// in this process is handed, whichever store it is: the identity header,
// every store's service token (so a client cannot claim the unrestricted
// identity of any store), this server's own service token, and the
// Authorization header that carried a session agent token — it addresses this
// gateway, not the store.
var requestHeadersNeverForwardedToStores = []string{
	requestcaller.PrincipalIdentityHeader,
	"X-Kanban-Store-Service-Token",
	"X-Grant-Store-Service-Token",
	"X-Reference-Store-Service-Token",
	requestcaller.ServiceTokenHeader,
	requestcaller.PeerTokenHeader,
	requestcaller.PeerConnectionCredentialHeader,
	"Authorization",
}
