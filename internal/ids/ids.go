// Package ids mints canonical bridge-server identifiers.
package ids

import "github.com/oklog/ulid/v2"

// NewMessageID returns a fresh canonical chat-message id (msg_<ULID>).
// ULIDs are lexicographically sortable and contain a millisecond timestamp,
// so ordering by id matches insertion order without consulting the clock.
func NewMessageID() string {
	return "msg_" + ulid.Make().String()
}
