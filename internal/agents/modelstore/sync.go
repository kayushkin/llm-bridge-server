package modelstore

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// The two paginated providers' base endpoints live here so a test can point
// syncAnthropic and syncGoogle at a stand-in server and read back what actually
// went on the wire. Overriding one is a test-only affordance; nothing in the
// package writes them.
var (
	anthropicModelsEndpoint = "https://api.anthropic.com/v1/models"
	googleModelsEndpoint    = "https://generativelanguage.googleapis.com/v1beta/models"
)

// SyncResult holds the outcome of a provider sync.
type SyncResult struct {
	Provider string
	Added    int
	Updated  int
	Err      error
}

// SyncProvider fetches models from a provider's API and upserts them into the store.
// apiKey is the bearer token / API key for authentication.
func (s *Store) SyncProvider(provider, apiKey string) (*SyncResult, error) {
	switch provider {
	case "anthropic":
		return s.syncAnthropic(apiKey)
	case "openai":
		return s.syncOpenAI(apiKey)
	case "google":
		return s.syncGoogle(apiKey)
	default:
		return nil, fmt.Errorf("sync not supported for provider %q", provider)
	}
}

// --- Anthropic ---

type anthropicModelsResponse struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	LastID  string           `json:"last_id"`
}

type anthropicModel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
	CreatedAt      string `json:"created_at"`
	Type           string `json:"type"`
}

func (s *Store) syncAnthropic(apiKey string) (*SyncResult, error) {
	result := &SyncResult{Provider: "anthropic"}

	var allModels []anthropicModel
	afterID := ""

	for {
		// after_id is a value the upstream minted and handed back; it goes
		// through url.Values rather than concatenation so that a token
		// carrying "+", "&" or "=" stays one parameter with its own bytes.
		query := neturl.Values{"limit": {"100"}}
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		requestURL := anthropicModelsEndpoint + "?" + query.Encode()

		req, err := http.NewRequest("GET", requestURL, nil)
		if err != nil {
			result.Err = err
			return result, err
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := httpClient().Do(req)
		if err != nil {
			result.Err = err
			return result, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			result.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncBody(body))
			return result, result.Err
		}

		var page anthropicModelsResponse
		if err := json.Unmarshal(body, &page); err != nil {
			result.Err = err
			return result, err
		}

		allModels = append(allModels, page.Data...)

		if !page.HasMore || page.LastID == "" {
			break
		}
		afterID = page.LastID
	}

	for _, m := range allModels {
		existing, _ := s.ResolveModel(m.ID)
		model := Model{
			ID:        m.ID,
			Provider:  "anthropic",
			Name:      m.DisplayName,
			MaxTokens: m.MaxInputTokens,
			Enabled:   true,
		}
		if existing != nil {
			model.adoptUserSetFieldsFrom(existing)
			// Name and MaxTokens keep the values the literal above took from
			// this answer: Anthropic sends both on every sync and is the
			// authority on them.
			result.Updated++
		} else {
			model.Priority = 100 // new models get default priority
			model.InputCost, model.OutputCost = guessAnthropicCost(m.ID)
			result.Added++
		}
		if err := s.AddModel(model); err != nil {
			result.Err = err
			return result, err
		}
	}

	return result, nil
}

func guessAnthropicCost(id string) (input, output float64) {
	switch {
	case strings.Contains(id, "fable"):
		return 10.0, 50.0
	case strings.Contains(id, "opus"):
		return 5.0, 25.0
	case strings.Contains(id, "sonnet"):
		return 3.0, 15.0
	case strings.Contains(id, "haiku"):
		return 1.0, 5.0
	default:
		return 3.0, 15.0
	}
}

// --- OpenAI ---

type openaiModelsResponse struct {
	Data []openaiModel `json:"data"`
}

type openaiModel struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// openaiRelevant filters to chat/reasoning models useful for coding agents.
// Excludes audio, realtime, transcribe, tts, search, fine-tuned, and legacy snapshot variants.
//
// A family is matched by its shape, not by a list of versions: "gpt-" followed
// by a version number, "o" followed by one, or "chatgpt-". A list of versions
// ("gpt-4", "gpt-5") went stale the day gpt-6 shipped and dropped every gpt-6
// id the API returned without a word. The shape also keeps out the non-chat
// "gpt-" lines (gpt-image, gpt-audio, gpt-realtime, gpt-live, gpt-transcribe),
// whose next word is not a number.
func openaiRelevant(id string) bool {
	for _, exclude := range []string{"audio", "realtime", "transcribe", "tts", "search", "diarize"} {
		if strings.Contains(id, exclude) {
			return false
		}
	}
	if strings.HasPrefix(id, "gpt-3.5") {
		return false
	}
	if strings.HasPrefix(id, "chatgpt-") {
		return true
	}
	if version, ok := strings.CutPrefix(id, "gpt-"); ok {
		return startsWithDigit(version)
	}
	if version, ok := strings.CutPrefix(id, "o"); ok {
		return startsWithDigit(version)
	}
	return false
}

func startsWithDigit(text string) bool {
	return text != "" && text[0] >= '0' && text[0] <= '9'
}

func (s *Store) syncOpenAI(apiKey string) (*SyncResult, error) {
	result := &SyncResult{Provider: "openai"}

	req, err := http.NewRequest("GET", "https://api.openai.com/v1/models", nil)
	if err != nil {
		result.Err = err
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient().Do(req)
	if err != nil {
		result.Err = err
		return result, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		result.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncBody(body))
		return result, result.Err
	}

	var page openaiModelsResponse
	if err := json.Unmarshal(body, &page); err != nil {
		result.Err = err
		return result, err
	}

	// Collect all IDs first to skip date-stamped snapshots when the base exists.
	idSet := map[string]bool{}
	for _, m := range page.Data {
		idSet[m.ID] = true
	}

	for _, m := range page.Data {
		if !openaiRelevant(m.ID) {
			continue
		}
		// Skip date-stamped snapshots (e.g. "gpt-4o-2024-08-06") when base model exists
		if isOpenAISnapshot(m.ID, idSet) {
			continue
		}
		existing, _ := s.ResolveModel(m.ID)
		model := Model{
			ID:       m.ID,
			Provider: "openai",
			Name:     m.ID,
			Enabled:  true,
		}
		if existing != nil {
			model.adoptUserSetFieldsFrom(existing)
			// Unlike Anthropic and Google, the OpenAI models endpoint returns
			// neither a display name nor a context window, so the stored values
			// are the only ones there are and must be carried over too.
			model.Name = existing.Name
			model.MaxTokens = existing.MaxTokens
			result.Updated++
		} else {
			model.Priority = 100
			model.MaxTokens, model.InputCost, model.OutputCost = guessOpenAICost(m.ID)
			result.Added++
		}
		if err := s.AddModel(model); err != nil {
			result.Err = err
			return result, err
		}
	}

	return result, nil
}

// isOpenAISnapshot returns true if the ID is a date-stamped snapshot (e.g. "gpt-4o-2024-08-06")
// and the base model (e.g. "gpt-4o") also exists in the set.
func isOpenAISnapshot(id string, idSet map[string]bool) bool {
	// Match patterns like "model-YYYY-MM-DD" or "model-YYYYMMDD"
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return false
	}
	// Try trimming date suffixes: "gpt-4o-2024-05-13" → check if "gpt-4o" exists
	// Date parts are 4-digit year or 2-digit month/day
	for i := len(parts) - 1; i >= 1; i-- {
		p := parts[i]
		isDatePart := len(p) >= 2 && len(p) <= 4 && p[0] >= '0' && p[0] <= '9'
		if !isDatePart {
			break
		}
		base := strings.Join(parts[:i], "-")
		if base != id && idSet[base] {
			return true
		}
	}
	return false
}

func guessOpenAICost(id string) (maxTokens int, input, output float64) {
	switch {
	case strings.HasPrefix(id, "o1-pro"):
		return 200000, 150.0, 600.0
	case strings.HasPrefix(id, "o3-mini"):
		return 200000, 1.10, 4.40
	case strings.HasPrefix(id, "o3"):
		return 200000, 2.0, 8.0
	case strings.HasPrefix(id, "o1"):
		return 200000, 15.0, 60.0
	case strings.HasPrefix(id, "o4-mini"):
		return 200000, 1.10, 4.40
	case strings.HasPrefix(id, "gpt-5-nano"):
		return 400000, 0.05, 0.40
	case strings.HasPrefix(id, "gpt-5-mini"):
		return 400000, 0.25, 2.00
	case strings.HasPrefix(id, "gpt-5-codex"):
		return 400000, 1.25, 10.0
	case strings.HasPrefix(id, "gpt-5"):
		return 400000, 1.25, 10.0
	case strings.HasPrefix(id, "gpt-4.1-nano"):
		return 1047576, 0.10, 0.40
	case strings.HasPrefix(id, "gpt-4.1-mini"):
		return 1047576, 0.40, 1.60
	case strings.HasPrefix(id, "gpt-4.1"):
		return 1047576, 2.0, 8.0
	case strings.HasPrefix(id, "gpt-4o-mini"):
		return 128000, 0.15, 0.60
	case strings.HasPrefix(id, "gpt-4o"):
		return 128000, 2.50, 10.0
	case strings.HasPrefix(id, "gpt-4-turbo"):
		return 128000, 10.0, 30.0
	case strings.HasPrefix(id, "gpt-4"):
		return 128000, 30.0, 60.0
	case strings.HasPrefix(id, "chatgpt-4o"):
		return 128000, 2.50, 10.0
	default:
		return 128000, 2.50, 10.0
	}
}

// --- Google ---

type googleModelsResponse struct {
	Models        []googleModel `json:"models"`
	NextPageToken string        `json:"nextPageToken"`
}

type googleModel struct {
	Name                       string   `json:"name"` // "models/gemini-2.5-pro"
	DisplayName                string   `json:"displayName"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

// googleRelevant filters to Gemini models that support generateContent.
func googleRelevant(m googleModel) bool {
	if !strings.Contains(m.Name, "gemini") {
		return false
	}
	for _, method := range m.SupportedGenerationMethods {
		if method == "generateContent" {
			return true
		}
	}
	return false
}

func (s *Store) syncGoogle(apiKey string) (*SyncResult, error) {
	result := &SyncResult{Provider: "google"}

	var allModels []googleModel
	pageToken := ""

	for {
		// key is a credential and pageToken is base64 the upstream minted;
		// both routinely carry "+", "/" and "=". A receiving server decodes a
		// raw "+" in a query string to a space, so concatenating either one
		// sends a value the holder never had, and the resulting failure names
		// nothing. url.Values escapes both.
		query := neturl.Values{"pageSize": {"100"}, "key": {apiKey}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		requestURL := googleModelsEndpoint + "?" + query.Encode()

		resp, err := httpClient().Get(requestURL)
		if err != nil {
			result.Err = err
			return result, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			result.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncBody(body))
			return result, result.Err
		}

		var page googleModelsResponse
		if err := json.Unmarshal(body, &page); err != nil {
			result.Err = err
			return result, err
		}

		allModels = append(allModels, page.Models...)

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	for _, m := range allModels {
		if !googleRelevant(m) {
			continue
		}
		// Normalize ID: "models/gemini-2.5-pro" → "gemini-2.5-pro"
		id := strings.TrimPrefix(m.Name, "models/")

		existing, _ := s.ResolveModel(id)
		model := Model{
			ID:        id,
			Provider:  "google",
			Name:      m.DisplayName,
			MaxTokens: m.InputTokenLimit,
			Enabled:   true,
		}
		if existing != nil {
			model.adoptUserSetFieldsFrom(existing)
			// Name and MaxTokens keep the values the literal above took from
			// this answer: Google sends both on every sync and is the authority
			// on them.
			result.Updated++
		} else {
			model.Priority = 100
			model.InputCost, model.OutputCost = guessGoogleCost(id)
			result.Added++
		}
		if err := s.AddModel(model); err != nil {
			result.Err = err
			return result, err
		}
	}

	return result, nil
}

func guessGoogleCost(id string) (input, output float64) {
	switch {
	case strings.Contains(id, "flash"):
		return 0.15, 0.60
	case strings.Contains(id, "pro"):
		return 1.25, 10.0
	default:
		return 1.25, 10.0
	}
}

// --- helpers ---

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// truncBody shortens a provider's HTTP error body for the error message the
// three sync paths build out of it. The three of them each pass a body straight
// from a provider API — Anthropic, OpenAI and Google all answer errors in JSON
// whose message field is free text, so a multi-byte rune landing on the cut is
// ordinary, not exotic.
func truncBody(b []byte) string {
	return truncateAtRuneBoundaryWithEllipsis(string(b), 200)
}

// truncateAtRuneBoundaryWithEllipsis returns s shortened to at most maxBytes
// bytes with "..." appended, cutting only between runes. A string already
// within budget comes back unchanged and gains no ellipsis, so the result runs
// up to maxBytes+3 bytes — the name says so because the ellipsis sits outside
// the budget, exactly as the byte cut it replaced had it.
//
// Cutting a Go string at a fixed byte offset splits whatever rune straddles
// that offset, and the result is not valid UTF-8. Nothing reports it: the split
// rune travels inside an error value to the ms CLI, which prints it, and a
// terminal renders the broken sequence as a replacement character with no
// failure raised anywhere on the path.
//
// The walk-back costs at most three byte comparisons and allocates nothing.
func truncateAtRuneBoundaryWithEllipsis(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is the first byte past the prefix. While it is a continuation
	// byte, a rune straddles the cut, so move the cut earlier.
	cut := maxBytes
	if cut < 0 {
		// A budget below zero keeps nothing, the same as a budget of zero. The
		// byte cut this replaced panicked here instead; in source a panic and a
		// split rune are the same expression, so no scan for one sees the
		// other. The only caller passes a literal 200.
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// adoptUserSetFieldsFrom copies across the fields a person owns, not the
// provider: everything a sync run must carry over from the row already in the
// store instead of taking from the API answer.
//
// It exists because AddModel writes with INSERT OR REPLACE, which deletes the
// old row and inserts a fresh one, so a field this method forgets is not merely
// left stale — it is erased on the next sync, silently, for every model the
// provider still lists. That is not hypothetical: short_name was reported
// disappearing from the live store before the copy existed.
//
// Every provider sync calls this, so the policy has one authoring. Metadata the
// provider is authoritative for — the display name and the context window — is
// deliberately NOT copied here, because the three providers disagree about it:
// Anthropic and Google send both on every answer and should win, while the
// OpenAI models endpoint sends neither, so its call site keeps the stored values
// itself. Adding a column to Model means adding it here or deciding, in writing,
// that the provider owns it.
func (m *Model) adoptUserSetFieldsFrom(existing *Model) {
	m.Enabled = existing.Enabled
	m.Priority = existing.Priority
	m.InputCost = existing.InputCost
	m.OutputCost = existing.OutputCost
	m.Aliases = existing.Aliases
	m.ShortName = existing.ShortName
}
