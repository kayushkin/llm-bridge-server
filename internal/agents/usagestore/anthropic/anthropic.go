// Package anthropic fetches Claude OAuth subscription usage from Anthropic.
//
// Mirrors what the Claude Code dashboard scrapes: the /api/oauth/usage endpoint
// returns rolling utilisation for the 5-hour and 7-day windows (plus per-model
// 7-day buckets for Opus/Sonnet, OAuth apps, cowork). Auth is the Claude OAuth
// access token from ~/.claude/.credentials.json (written by the Claude Code CLI).
package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

const (
	usageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	betaHeader    = "oauth-2025-04-20"
)

// Window names exposed in ProviderLimits.Windows.
const (
	WindowFiveHour      = "five_hour"
	WindowSevenDay      = "seven_day"
	WindowSevenDayOAuth = "seven_day_oauth_apps"
	WindowSevenDayOpus  = "seven_day_opus"
	WindowSevenDaySonnet = "seven_day_sonnet"
	WindowSevenDayCowork = "seven_day_cowork"
	WindowExtraUsage    = "extra_usage"
)

// rawWindow matches the wire format. Utilization is 0–100. resets_at is ISO-8601.
type rawWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

type rawExtra struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

type rawResponse struct {
	FiveHour       *rawWindow `json:"five_hour"`
	SevenDay       *rawWindow `json:"seven_day"`
	SevenDayOAuth  *rawWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus   *rawWindow `json:"seven_day_opus"`
	SevenDaySonnet *rawWindow `json:"seven_day_sonnet"`
	SevenDayCowork *rawWindow `json:"seven_day_cowork"`
	ExtraUsage     *rawExtra  `json:"extra_usage"`
}

// Credentials holds the relevant subset of ~/.claude/.credentials.json.
type Credentials struct {
	AccessToken      string
	RefreshToken     string
	SubscriptionType string
	RateLimitTier    string
}

// LoadCredentials reads ~/.claude/.credentials.json.
func LoadCredentials() (*Credentials, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var wire struct {
		ClaudeAiOauth struct {
			AccessToken      string `json:"accessToken"`
			RefreshToken     string `json:"refreshToken"`
			SubscriptionType string `json:"subscriptionType"`
			RateLimitTier    string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if wire.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("no oauth access token in %s", path)
	}
	return &Credentials{
		AccessToken:      wire.ClaudeAiOauth.AccessToken,
		RefreshToken:     wire.ClaudeAiOauth.RefreshToken,
		SubscriptionType: wire.ClaudeAiOauth.SubscriptionType,
		RateLimitTier:    wire.ClaudeAiOauth.RateLimitTier,
	}, nil
}

// Collector fetches and normalizes Anthropic subscription limits.
type Collector struct {
	HTTPClient *http.Client
}

// New returns a Collector with sane defaults.
func New() *Collector {
	return &Collector{HTTPClient: &http.Client{Timeout: 10 * time.Second}}
}

// Fetch hits Anthropic's OAuth usage endpoint and returns a normalized snapshot
// alongside the raw response body (for storage / debugging).
func (c *Collector) Fetch() (*usagestore.ProviderLimits, []byte, error) {
	creds, err := LoadCredentials()
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest("GET", usageEndpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("anthropic-beta", betaHeader)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("anthropic API: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, body, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, body, fmt.Errorf("anthropic API returned %d: %s", resp.StatusCode, string(body))
	}

	var raw rawResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, body, fmt.Errorf("parse response: %w", err)
	}

	out := &usagestore.ProviderLimits{
		Provider:   "anthropic",
		PlanType:   creds.SubscriptionType,
		Tier:       creds.RateLimitTier,
		SnapshotAt: time.Now().Unix(),
		Source:     "api",
		Windows:    map[string]*usagestore.LimitWindow{},
	}
	addWindow := func(key string, w *rawWindow) {
		if w == nil {
			return
		}
		out.Windows[key] = &usagestore.LimitWindow{
			UsedPercent: w.Utilization,
			ResetsAt:    parseISO(w.ResetsAt),
		}
	}
	addWindow(WindowFiveHour, raw.FiveHour)
	addWindow(WindowSevenDay, raw.SevenDay)
	addWindow(WindowSevenDayOAuth, raw.SevenDayOAuth)
	addWindow(WindowSevenDayOpus, raw.SevenDayOpus)
	addWindow(WindowSevenDaySonnet, raw.SevenDaySonnet)
	addWindow(WindowSevenDayCowork, raw.SevenDayCowork)

	if raw.ExtraUsage != nil && raw.ExtraUsage.IsEnabled && raw.ExtraUsage.Utilization != nil {
		out.Windows[WindowExtraUsage] = &usagestore.LimitWindow{UsedPercent: *raw.ExtraUsage.Utilization}
	}

	return out, body, nil
}

func parseISO(s *string) *int64 {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	v := t.Unix()
	return &v
}
