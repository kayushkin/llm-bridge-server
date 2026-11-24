// Package spend computes per-API-key spend for Claude API keys. The dollar
// figure is derived locally from token counts × per-model pricing, not from
// the org cost_report (which can't filter by api_key_id and lumps in
// imputed Max-subscription value).
package spend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

const (
	anthropicAPIKeysURL = "https://api.anthropic.com/v1/organizations/api_keys"
	anthropicUsageURL   = "https://api.anthropic.com/v1/organizations/usage_report/messages"
	anthropicAPIVersion = "2023-06-01"

	// FetchDays is how far back we pull usage_report buckets. Has to cover
	// the oldest top-up date users care about; 90 days is generous and only
	// costs one extra page on the Anthropic side.
	FetchDays = 90
)

// AnthropicResult is the full output of a single Fetch — metadata for every
// API key on the org plus daily-bucketed cost rows. Save to the store as:
//
//	for each k in Keys:    SaveKeyMeta(k)
//	for each d in Daily:   SaveDailySpend(d)
type AnthropicResult struct {
	AdminKeyHint string
	Keys         []usagestore.KeyMeta
	Daily        []usagestore.DailySpend
}

// AnthropicKeyCollector lists every API key on the org and computes spend
// per (key, day) from usage_report/messages. The admin key is fetched lazily
// so rotation is picked up without a process restart.
type AnthropicKeyCollector struct {
	APIKeyFn   func() (string, error)
	HTTPClient *http.Client
	// Logger receives one line per unknown model, so the user notices when a
	// new model needs adding to the pricing table.
	OnUnknownModel func(model string)
}

func NewAnthropicKey(apiKeyFn func() (string, error)) *AnthropicKeyCollector {
	return &AnthropicKeyCollector{
		APIKeyFn:   apiKeyFn,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// rawAPIKey is the slice of /v1/organizations/api_keys we keep.
type rawAPIKey struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	PartialKeyHint string `json:"partial_key_hint"`
	Status         string `json:"status"`
}

type rawAPIKeysResponse struct {
	Data    []rawAPIKey `json:"data"`
	HasMore bool        `json:"has_more"`
	LastID  string      `json:"last_id"`
}

type rawUsageBucket struct {
	StartingAt string           `json:"starting_at"`
	EndingAt   string           `json:"ending_at"`
	Results    []rawUsageResult `json:"results"`
}

type rawCacheCreation struct {
	Ephemeral5mInput int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInput int64 `json:"ephemeral_1h_input_tokens"`
}

type rawUsageResult struct {
	UncachedInput int64            `json:"uncached_input_tokens"`
	CacheRead     int64            `json:"cache_read_input_tokens"`
	CacheCreation rawCacheCreation `json:"cache_creation"`
	OutputTokens  int64            `json:"output_tokens"`
	APIKeyID      string           `json:"api_key_id"`
	Model         string           `json:"model"`
}

type rawUsageResponse struct {
	Data     []rawUsageBucket `json:"data"`
	HasMore  bool             `json:"has_more"`
	NextPage string           `json:"next_page"`
}

// Fetch returns metadata for every key + one daily-cost row per (key, day).
// One usage_report request covers the full FetchDays window; we partition
// per-key locally to stay inside Anthropic's tight admin-API rate limits.
func (c *AnthropicKeyCollector) Fetch() (*AnthropicResult, error) {
	adminHint, err := c.adminKeyHint()
	if err != nil {
		return nil, fmt.Errorf("admin key: %w", err)
	}

	keys, err := c.listKeys()
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}

	end := time.Now().UTC().Truncate(24 * time.Hour) // today 00:00 UTC
	start := end.AddDate(0, 0, -FetchDays)

	usage, rawByKey, err := c.fetchUsage(start, end)
	if err != nil {
		return nil, fmt.Errorf("usage_report: %w", err)
	}

	now := time.Now().Unix()
	res := &AnthropicResult{
		AdminKeyHint: adminHint,
		Keys:         make([]usagestore.KeyMeta, 0, len(keys)),
	}
	for _, k := range keys {
		res.Keys = append(res.Keys, usagestore.KeyMeta{
			Provider:     "anthropic",
			APIKeyID:     k.ID,
			APIKeyName:   k.Name,
			APIKeyHint:   k.PartialKeyHint,
			APIKeyStatus: k.Status,
			RawJSON:      rawByKey[k.ID],
			FetchedAt:    now,
		})
	}

	daily, err := dailyFromBuckets(usage, now, c.OnUnknownModel)
	if err != nil {
		return nil, fmt.Errorf("usage_report: %w", err)
	}
	res.Daily = daily
	return res, nil
}

// bucketDate turns a usage_report bucket's starting_at into the YYYY-MM-DD key
// SaveDailySpend rows are stored under.
//
// It parses rather than slices. The obvious spelling is starting_at[:10], and
// that is wrong twice over: it panics outright on a value shorter than ten
// bytes (an omitted starting_at is the empty string, and Go's zero value gives
// no hint that the field was ever missing), and on anything else it produces a
// ten-byte prefix that is not checked to be a date at all. The prefix then
// becomes the primary key half of a spend row, so a malformed timestamp would
// quietly file real dollars under a nonsense day and go on matching itself on
// every later fetch. Truncating the string more carefully does not help — the
// value is parsed as data, not printed, so the only honest repair is to refuse
// a bucket whose date we cannot read.
func bucketDate(startingAt string) (string, error) {
	t, err := time.Parse(time.RFC3339, startingAt)
	if err != nil {
		return "", fmt.Errorf("bucket starting_at %q: %w", startingAt, err)
	}
	return t.UTC().Format("2006-01-02"), nil
}

// dailyFromBuckets partitions usage_report buckets into one cost row per
// (api key, day). Buckets where a key had no token activity yield no row.
func dailyFromBuckets(usage []rawUsageBucket, fetchedAt int64, onUnknownModel func(string)) ([]usagestore.DailySpend, error) {
	daily := make([]usagestore.DailySpend, 0, len(usage))
	for _, b := range usage {
		date, err := bucketDate(b.StartingAt)
		if err != nil {
			return nil, err
		}
		perKey := map[string]float64{}
		for _, r := range b.Results {
			perKey[r.APIKeyID] += costFromTokens(r, onUnknownModel)
		}
		for apiKeyID, cost := range perKey {
			daily = append(daily, usagestore.DailySpend{
				Provider:  "anthropic",
				APIKeyID:  apiKeyID,
				Date:      date,
				TotalUSD:  cost,
				FetchedAt: fetchedAt,
			})
		}
	}
	return daily, nil
}

// adminKeyHint returns "sk-ant-admin01-…" style fingerprint for display.
// 14 chars + ellipsis + last 4 keeps it identifiable without logging the
// secret.
//
// Both cuts land on a rune boundary. The hint travels to SaveAdminHint and out
// again as the admin_key_hint field of GET /api/spend/keys, so a cut through a
// multi-byte rune would put a replacement character on :8185's wire with no
// error raised anywhere — encoding/json substitutes U+FFFD rather than
// failing. Every Anthropic admin-key format is ASCII, so this is unswept
// rather than on fire.
//
// The hint is display-only: nothing parses it and nothing keys on it, so a
// budget shortened by up to three bytes is the right repair here. A cut whose
// result were parsed or used as a key would need a refusal instead.
//
// A key too short to cut is masked entirely rather than returned. Returning it
// would publish the secret on the one field whose whole purpose is to avoid
// publishing it — the hint is served as admin_key_hint by GET /api/spend/keys,
// so the branch leaks over HTTP and not merely into a log. No real Anthropic
// admin key is this short, so the branch fires on a truncated, placeholder or
// misconfigured key, which is also when someone is most likely to be looking at
// the spend page. All-stars is the answer auth-store.MaskSecret, aiauth.MaskKey
// and apiauth.MaskToken already give for the same case; usage-store was the one
// site on this box that dissented.
func (c *AnthropicKeyCollector) adminKeyHint() (string, error) {
	k, err := c.APIKeyFn()
	if err != nil {
		return "", err
	}
	if len(k) < 24 {
		// Star count is the byte length, matching the three siblings named
		// above. It concedes the key's length, which those already concede,
		// and no admin key reaching this branch is a valid credential anyway.
		return strings.Repeat("*", len(k)), nil
	}
	return truncateAtRuneBoundary(k, 14) + "..." + suffixAtRuneBoundary(k, 4), nil
}

func (c *AnthropicKeyCollector) listKeys() ([]rawAPIKey, error) {
	apiKey, err := c.APIKeyFn()
	if err != nil {
		return nil, err
	}
	out := []rawAPIKey{}
	afterID := ""
	for {
		u, _ := url.Parse(anthropicAPIKeysURL)
		q := u.Query()
		q.Set("limit", "100")
		if afterID != "" {
			q.Set("after_id", afterID)
		}
		u.RawQuery = q.Encode()

		req, _ := http.NewRequest("GET", u.String(), nil)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("http: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(body))
		}
		var raw rawAPIKeysResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		out = append(out, raw.Data...)
		if !raw.HasMore || raw.LastID == "" {
			break
		}
		afterID = raw.LastID
	}
	return out, nil
}

// fetchUsage retrieves all daily buckets for [start, end] grouped by api_key
// and model. Returns the merged bucket list plus a per-key pretty-printed
// raw JSON snapshot for the /raw endpoint.
func (c *AnthropicKeyCollector) fetchUsage(start, end time.Time) ([]rawUsageBucket, map[string]string, error) {
	apiKey, err := c.APIKeyFn()
	if err != nil {
		return nil, nil, err
	}
	all := []rawUsageBucket{}
	page := ""
	for {
		u, _ := url.Parse(anthropicUsageURL)
		q := u.Query()
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("ending_at", end.Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Add("group_by[]", "api_key_id")
		q.Add("group_by[]", "model")
		if page != "" {
			q.Set("page", page)
		}
		u.RawQuery = q.Encode()

		req, _ := http.NewRequest("GET", u.String(), nil)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("http: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(body))
		}
		var raw rawUsageResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, nil, fmt.Errorf("parse: %w", err)
		}
		all = append(all, raw.Data...)
		if !raw.HasMore || raw.NextPage == "" {
			break
		}
		page = raw.NextPage
	}

	// Build a per-key raw JSON snapshot so the UI can show "raw" without a
	// second API call.
	rawByKey := map[string]string{}
	byKey := map[string][]rawUsageBucket{}
	for _, b := range all {
		perKey := map[string][]rawUsageResult{}
		for _, r := range b.Results {
			perKey[r.APIKeyID] = append(perKey[r.APIKeyID], r)
		}
		for kid, results := range perKey {
			byKey[kid] = append(byKey[kid], rawUsageBucket{
				StartingAt: b.StartingAt,
				EndingAt:   b.EndingAt,
				Results:    results,
			})
		}
	}
	for kid, buckets := range byKey {
		b, _ := json.MarshalIndent(map[string]interface{}{
			"api_key_id": kid,
			"buckets":    buckets,
		}, "", "  ")
		rawByKey[kid] = string(b)
	}

	return all, rawByKey, nil
}

// costFromTokens applies pricing × per-model multipliers. Unknown models tally
// $0 and trigger onUnknown so the gap is visible.
func costFromTokens(r rawUsageResult, onUnknown func(string)) float64 {
	p, ok := LookupAnthropic(r.Model)
	if !ok {
		if onUnknown != nil {
			onUnknown(r.Model)
		}
		return 0
	}
	const M = 1_000_000.0
	cost := 0.0
	cost += float64(r.UncachedInput) / M * p.InputUSDPerMTok
	cost += float64(r.OutputTokens) / M * p.OutputUSDPerMTok
	cost += float64(r.CacheRead) / M * p.CacheReadPerMTok()
	cost += float64(r.CacheCreation.Ephemeral5mInput) / M * p.CacheWrite5mPerMTok()
	cost += float64(r.CacheCreation.Ephemeral1hInput) / M * p.CacheWrite1hPerMTok()
	return cost
}

// parseAmountString is kept exported in case raw amounts ever come back as
// numeric strings on a future field. Currently unused.
func parseAmountString(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}
