// Package clientrequestids mints the per-turn id a starter puts in
// msg.SendMessageRequest.ClientRequestID. llm-bridge-server stamps it on every
// event of the turn the message opens, so the turn can be matched to the
// request that opened it.
//
// The scheduler's agent jobs and the job binaries used to publish their turns
// on NATS, and llm-bridge-adapter minted this id for each turn that came
// without one. They now call the bridge's session API themselves, and mint it
// here in the same form, so their turns carry one as before.
package clientrequestids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns "req_" and 16 random hex digits.
func New() (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("mint a client request id: %w", err)
	}
	return "req_" + hex.EncodeToString(randomBytes[:]), nil
}
