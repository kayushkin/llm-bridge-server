package harness

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

// resolveAuthCredential looks up a credential by its bridge ID
// (auth-store credential id) and resolves the live secret value.
// nil if the ID is empty or auth-store has no matching record —
// callers handle that as "no usable credential" without crashing.
func resolveAuthCredential(client *authstoreclient.Client, credentialID, reason string) *authstoreclient.Resolved {
	if credentialID == "" || client == nil {
		return nil
	}
	if reason == "" {
		reason = "harness:resolve"
	}
	r, err := client.Resolve(context.Background(), credentialID, reason)
	if err != nil {
		return nil
	}
	return r
}
