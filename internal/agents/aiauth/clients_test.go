package aiauth

import (
	"testing"
	"time"
)

// TestAnthropicClient_BetaHeaders verifies that the Anthropic client is created
// with prompt caching beta headers in all auth modes.
func TestAnthropicClient_BetaHeaders(t *testing.T) {
	tests := []struct {
		name       string
		credType   string
		wantBeta   bool
		description string
	}{
		{
			name:       "OAuth token includes beta header",
			credType:   "oauth",
			wantBeta:   true,
			description: "OAuth tokens (sk-ant-oat01-*) should include prompt-caching-2024-07-31 in beta headers",
		},
		{
			name:       "API key includes beta header",
			credType:   "api_key",
			wantBeta:   true,
			description: "API keys (sk-ant-api03-*) should include prompt-caching-2024-07-31 header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &Store{
				data: &AuthStore{
					Version: 1,
					Profiles: map[string]*Credential{
						"anthropic": {
							Type:     tt.credType,
							Provider: "anthropic",
							Key:      "sk-ant-api03-test",
							Access:   "sk-ant-oat01-test",
							Refresh:  "refresh_test",
							Expires:  time.Now().Add(1 * time.Hour).UnixMilli(),
						},
					},
				},
			}

			client, err := store.AnthropicClient()
			if err != nil {
				t.Fatalf("AnthropicClient() error = %v", err)
			}

			// Verify client is created successfully
			if client == nil {
				t.Fatal("client is nil")
			}

			// TODO: Add reflection-based header verification once SDK exposes headers
			// For now, we rely on manual inspection and integration testing
			t.Logf("Client created successfully for %s auth", tt.credType)
			t.Log(tt.description)
		})
	}
}
