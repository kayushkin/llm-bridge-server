package aiauth

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicClient returns an authenticated *anthropic.Client.
// OAuth tokens (sk-ant-oat01-*) use Bearer auth with required beta headers.
// API keys (sk-ant-api03-*) use x-api-key header.
func (s *Store) AnthropicClient() (*anthropic.Client, error) {
	key, err := s.ResolveKey("anthropic")
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(key, "sk-ant-oat01-") {
		// OAuth tokens require Bearer auth + beta headers.
		// Explicitly clear apiKey to prevent SDK's DefaultClientOptions from
		// also sending x-api-key (which the server would treat as a no-credits API key).
		c := anthropic.NewClient(
			option.WithAPIKey(""),
			option.WithAuthToken(key),
			option.WithHeader("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,prompt-caching-2024-07-31"),
			option.WithHeader("user-agent", "claude-cli/2.1.44 (external, cli)"),
			option.WithHeader("x-app", "cli"),
		)
		return &c, nil
	}

	c := anthropic.NewClient(
		option.WithAPIKey(key),
		option.WithHeader("anthropic-beta", "prompt-caching-2024-07-31"),
	)
	return &c, nil
}
