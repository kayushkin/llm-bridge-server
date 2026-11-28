// Package accesstokens makes the random secrets this server hands out and
// the hashes it keeps of them.
package accesstokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// RandomURLSafe returns n bytes from crypto/rand, base64url-encoded. A
// failure of the system's random source is not something to work around.
func RandomURLSafe(n int) (string, error) {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("read crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// SHA256Hex is how a token is kept and looked up: never the token itself.
func SHA256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
