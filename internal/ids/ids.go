// Package ids mints canonical bridge-server identifiers.
package ids

import "github.com/oklog/ulid/v2"

// NewMessageID returns a fresh canonical chat-message id (msg_<ULID>).
// ULIDs are lexicographically sortable and contain a millisecond timestamp,
// so ordering by id matches insertion order without consulting the clock.
func NewMessageID() string {
	return "msg_" + ulid.Make().String()
}

// NewTurnID returns a fresh canonical turn id (turn_<ULID>). A turn spans
// from a user_message through every event it produces until the terminating
// result/error.
func NewTurnID() string {
	return "turn_" + ulid.Make().String()
}

// NewHookID returns a fresh canonical hook registry id (hook_<ULID>).
func NewHookID() string {
	return "hook_" + ulid.Make().String()
}
