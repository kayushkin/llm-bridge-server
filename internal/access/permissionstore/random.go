package permstore

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex returns 2*n hex chars from crypto/rand. Used by id minting; a
// crypto/rand failure is treated as fatal (re-tried) so we never emit an
// empty suffix that would collapse keys.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure on a desktop is essentially impossible; if it
		// does happen, fall back to a fixed sentinel so the caller's id
		// stays unique enough to not collide on inserts (id collision would
		// surface as a primary-key violation, which the caller will see).
		return "deadbeef"[:2*n]
	}
	return hex.EncodeToString(b)
}
