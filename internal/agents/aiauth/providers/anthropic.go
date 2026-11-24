package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/aiauth"
)

const (
	AnthropicClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AnthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	AnthropicTokenURL     = "https://console.anthropic.com/v1/oauth/token"
	AnthropicRedirectURI  = "https://console.anthropic.com/oauth/code/callback"
	AnthropicScopes       = "org:create_api_key user:profile user:inference"
)

// Anthropic implements the aiauth.Provider interface.
type Anthropic struct{}

func NewAnthropic() *Anthropic { return &Anthropic{} }

func (a *Anthropic) ID() string { return "anthropic" }

func (a *Anthropic) Login(cb aiauth.LoginCallbacks) (*aiauth.Credential, error) {
	verifier, challenge, err := aiauth.GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("PKCE generation failed: %w", err)
	}

	params := url.Values{
		"code":                  {"true"},
		"client_id":             {AnthropicClientID},
		"redirect_uri":         {AnthropicRedirectURI},
		"response_type":        {"code"},
		"scope":                {AnthropicScopes},
		"code_challenge":       {challenge},
		"code_challenge_method": {"S256"},
		"state":                {verifier},
	}
	authURL := AnthropicAuthorizeURL + "?" + params.Encode()

	if cb.OnAuthURL != nil {
		if err := cb.OnAuthURL(authURL); err != nil {
			return nil, err
		}
	}

	if cb.OnPrompt == nil {
		return nil, fmt.Errorf("OnPrompt callback required")
	}

	code, err := cb.OnPrompt("Paste the authorization code:")
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)

	// Parse code#state format (matches pi-ai flow)
	parts := strings.SplitN(code, "#", 2)
	authCode := parts[0]
	state := ""
	if len(parts) > 1 {
		state = parts[1]
	}

	return a.exchangeCode(authCode, state, verifier)
}

func (a *Anthropic) exchangeCode(code, state, verifier string) (*aiauth.Credential, error) {
	// Use JSON body (not form-encoded) to match pi-ai's flow
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     AnthropicClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  AnthropicRedirectURI,
		"code_verifier": verifier,
	}
	jsonBody, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", AnthropicTokenURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aiauth/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	// 5 minute buffer before expiry (matches pi-ai)
	expiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000 - 5*60*1000

	return &aiauth.Credential{
		Type:    "oauth",
		Provider: "anthropic",
		Access:  tokenResp.AccessToken,
		Refresh: tokenResp.RefreshToken,
		Expires: expiresAt,
	}, nil
}

func (a *Anthropic) RefreshToken(cred *aiauth.Credential) (*aiauth.Credential, error) {
	if cred.Refresh == "" {
		return nil, fmt.Errorf("no refresh token available")
	}

	// Use JSON body with User-Agent (Cloudflare blocks bare requests)
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     AnthropicClientID,
		"refresh_token": cred.Refresh,
	}
	jsonBody, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", AnthropicTokenURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aiauth/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token refresh returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	refreshToken := tokenResp.RefreshToken
	if refreshToken == "" {
		refreshToken = cred.Refresh
	}

	expiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000 - 5*60*1000

	return &aiauth.Credential{
		Type:     "oauth",
		Provider: "anthropic",
		Access:   tokenResp.AccessToken,
		Refresh:  refreshToken,
		Expires:  expiresAt,
		Email:    cred.Email,
	}, nil
}
