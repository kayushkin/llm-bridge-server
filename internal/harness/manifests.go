package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// ManifestContext is the bridge-side state a manifest builder needs to
// produce a HarnessService for a particular runner. Carries everything
// per-spawn — runner host info, server URL the runner will pull binaries
// from, and the resolved credential bound to the instance.
type ManifestContext struct {
	ServerURL  string                    // public bridge URL (used in BinaryURL)
	OS         string                    // runner GOOS
	Arch       string                    // runner GOARCH
	Credential *authstoreclient.Resolved // resolved credential bound to the instance, may be nil
	AuthClient *authstoreclient.Client   // for fallback resolution when Credential is nil
	Reason     string                    // audit reason ("session:abc:harness:foo")
}

// BuildProvision returns the list of HarnessServices the runner should
// ensure are running before forking the wrapper for the given harness.
// Each harness type has a manifest builder that knows what backend, if
// any, it depends on. Wrapper-CLI harnesses (claude_code, codex…) return
// nil — they shell out to a CLI on PATH and have no daemon to deploy.
func BuildProvision(harness msg.Harness, ctx ManifestContext) ([]msg.HarnessService, error) {
	if b := manifestBuilders[harness]; b != nil {
		return b(ctx)
	}
	return nil, nil
}

type manifestBuilder func(ctx ManifestContext) ([]msg.HarnessService, error)

// manifestBuilders maps harness types to their deployment recipes.
var manifestBuilders = map[msg.Harness]manifestBuilder{
	msg.HarnessInber: buildInberManifest,
}

// buildInberManifest emits a single inber-server backend service. The
// runner downloads the cross-compiled binary, runs it with the resolved
// Anthropic key in env, and waits for /health.
func buildInberManifest(ctx ManifestContext) ([]msg.HarnessService, error) {
	apiKey, err := pickAnthropicKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("inber needs an Anthropic credential: %w", err)
	}
	return []msg.HarnessService{{
		Name:      "inber-server",
		BinaryURL: assetURL(ctx.ServerURL, "inber-server", ctx.OS, ctx.Arch),
		Args:      []string{},
		Env: []string{
			"ANTHROPIC_API_KEY=" + apiKey,
		},
		HealthURL: "http://localhost:8200/health",
	}}, nil
}

// pickAnthropicKey extracts an Anthropic API key. Preference order:
//
//  1. The explicitly-bound credential resolved by ctx.Credential.
//  2. Best-match provider=anthropic resolution via auth-store, preferring
//     api_key creds (most predictable, no refresh dance).
func pickAnthropicKey(ctx ManifestContext) (string, error) {
	if ctx.Credential != nil {
		return anthropicSecret(ctx.Credential)
	}
	if ctx.AuthClient == nil {
		return "", fmt.Errorf("no auth-store client available")
	}
	reason := ctx.Reason
	if reason == "" {
		reason = "manifest:inber"
	}
	c, err := ctx.AuthClient.ResolveByProvider(context.Background(), "anthropic", "", "llm-bridge-server", reason)
	if err != nil {
		return "", fmt.Errorf("no anthropic credential available: %w", err)
	}
	return anthropicSecret(c)
}

// anthropicSecret returns the usable secret string from a resolved
// auth-store credential.
func anthropicSecret(c *authstoreclient.Resolved) (string, error) {
	if c == nil {
		return "", fmt.Errorf("nil credential")
	}
	if c.Provider != "" && c.Provider != "anthropic" {
		return "", fmt.Errorf("expected anthropic credential, got provider=%q", c.Provider)
	}
	if s := c.Secret(); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("credential has no usable secret")
}

// assetURL composes the canonical /api/runner/binary URL for a backend
// service binary.
func assetURL(serverURL, name, os, arch string) string {
	base := strings.TrimRight(serverURL, "/") + "/api/runner/binary"
	q := fmt.Sprintf("?name=%s&os=%s&arch=%s", name, os, arch)
	return base + q
}
