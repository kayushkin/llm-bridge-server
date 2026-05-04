// Package ids mints canonical bridge-server identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/oklog/ulid/v2"
)

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

// NewHookRequestID returns a fresh per-invocation hook request id
// (hreq_<hex16>). Used to correlate awaiting_resolution → completed
// HookEvents and to key parked permission asks.
func NewHookRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Crypto rand failure is exceptional — fall back to a sentinel
		// rather than panicking; the audit log will still be unique-ish
		// across invocations because the harness drains pending entries
		// on resolve, but flag the failure so it's visible.
		return "hreq_unseeded"
	}
	return "hreq_" + hex.EncodeToString(b)
}
