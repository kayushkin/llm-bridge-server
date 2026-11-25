// Package httpapi serves usage-store's routes: subscription limits, per-key
// spend and top-ups, and the old token totals. The server mounts it under
// /usage-store/ and hands it each request with that prefix taken off, so the
// paths are the ones the standalone service answered.
package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	usagestore "github.com/kayushkin/llm-bridge-server/internal/agents/usagestore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/anthropic"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/boundedtext"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/codex"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/spend"
)

// Server answers usage-store's routes. All but /health live under /api/usage.
type Server struct {
	store          *usagestore.Store
	tokenUsage     TokenUsageReader
	anthropic      *anthropic.Collector
	codex          *codex.Reader
	limitsWatch    LimitsWatch
	limitsStaleAge time.Duration
	spendAnthropic *spend.AnthropicKeyCollector // nil when no admin key was discovered
	mux            *http.ServeMux

	// The per-provider admin-key hint from the most recent spend fetch,
	// written by the spend refresher and read by /spend/keys. Empty until the
	// first fetch completes.
	adminHintsMutex sync.Mutex
	adminHints      map[string]string
}

// LimitsWatch is told when the usage page is being looked at.
type LimitsWatch interface {
	MarkWatched()
}

// TokenUsageTotals is one agent, orchestrator and model's token totals over a
// period, as GET /api/usage answers them.
type TokenUsageTotals struct {
	Agent        string  `json:"agent"`
	Orchestrator string  `json:"orchestrator"`
	Model        string  `json:"model"`
	Messages     int     `json:"messages"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// TokenUsageReader reads the daily token counts an older orchestrator wrote
// into model-store: the totals of each agent, orchestrator and model over the
// last days days, the largest first.
type TokenUsageReader interface {
	TokenUsageOverTheLastDays(days int) ([]TokenUsageTotals, error)
}

type SpendCollectors struct {
	Anthropic *spend.AnthropicKeyCollector
}

// New builds the server. limitsStaleAge is how old a limits snapshot may be
// before responses mark it stale. cx is nil when codex limits are off.
func New(s *usagestore.Store, tokenUsage TokenUsageReader, ant *anthropic.Collector, cx *codex.Reader, limitsWatch LimitsWatch, limitsStaleAge time.Duration, sc SpendCollectors) *Server {
	srv := &Server{
		store:          s,
		tokenUsage:     tokenUsage,
		anthropic:      ant,
		codex:          cx,
		limitsWatch:    limitsWatch,
		limitsStaleAge: limitsStaleAge,
		spendAnthropic: sc.Anthropic,
		adminHints:     map[string]string{},
		mux:            http.NewServeMux(),
	}
	srv.routes()
	return srv
}

// SaveAdminHint records the per-provider admin-key partial-hint discovered on
// the latest fetch. Called by the spend refresher after a successful Fetch.
func (s *Server) SaveAdminHint(provider, hint string) {
	s.adminHintsMutex.Lock()
	defer s.adminHintsMutex.Unlock()
	s.adminHints[provider] = hint
}

func (s *Server) adminHint(provider string) string {
	s.adminHintsMutex.Lock()
	defer s.adminHintsMutex.Unlock()
	return s.adminHints[provider]
}

// RoutePatterns are the patterns routes registers, which the server mounts
// one by one under /usage-store/.
var RoutePatterns = []string{
	"GET /api/usage",
	"GET /api/usage/limits",
	"GET /api/usage/limits/{provider}",
	"GET /api/usage/limits/{provider}/history",
	"POST /api/usage/limits/refresh",
	"POST /api/usage/limits/watch",
	"GET /api/usage/spend/keys",
	"GET /api/usage/spend/keys/{provider}/{api_key_id}/raw",
	"POST /api/usage/spend/refresh",
	"GET /api/usage/spend/topups",
	"POST /api/usage/spend/topups",
	"DELETE /api/usage/spend/topups/{id}",
	"GET /health",
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/usage", s.handleUsage)
	s.mux.HandleFunc("GET /api/usage/limits", s.handleLimits)
	s.mux.HandleFunc("GET /api/usage/limits/{provider}", s.handleProviderLimits)
	s.mux.HandleFunc("GET /api/usage/limits/{provider}/history", s.handleHistory)
	s.mux.HandleFunc("POST /api/usage/limits/refresh", s.handleRefresh)
	s.mux.HandleFunc("POST /api/usage/limits/watch", s.handleLimitsWatch)
	s.mux.HandleFunc("GET /api/usage/spend/keys", s.handleSpendKeys)
	s.mux.HandleFunc("GET /api/usage/spend/keys/{provider}/{api_key_id}/raw", s.handleSpendKeyRaw)
	s.mux.HandleFunc("POST /api/usage/spend/refresh", s.handleSpendRefresh)
	s.mux.HandleFunc("GET /api/usage/spend/topups", s.handleListTopups)
	s.mux.HandleFunc("POST /api/usage/spend/topups", s.handleAddTopup)
	s.mux.HandleFunc("DELETE /api/usage/spend/topups/{id}", s.handleDeleteTopup)
	s.mux.HandleFunc("GET /health", s.handleHealth)
}

// ServeHTTP answers one request whose path has no /usage-store prefix.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ---- token-usage aggregates (legacy) ----

// handleUsage answers the totals over the last day, week and month.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.tokenUsage == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("this server has no model-store to read token usage from"))
		return
	}
	periods := map[string][]TokenUsageTotals{}
	for name, days := range map[string]int{"day": 1, "week": 7, "month": 30} {
		rows, err := s.tokenUsage.TokenUsageOverTheLastDays(days)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("period query (%d days): %w", days, err))
			return
		}
		if rows == nil {
			rows = []TokenUsageTotals{}
		}
		periods[name] = rows
	}
	writeJSON(w, periods)
}

// ---- subscription limits (legacy) ----

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	out := map[string]*usagestore.ProviderLimits{}
	for _, p := range []string{"anthropic", "codex"} {
		snap, err := s.store.LatestLimits(p)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("latest %s: %w", p, err))
			return
		}
		s.markStaleness(snap)
		out[p] = snap
	}
	writeJSON(w, out)
}

// markStaleness sets StaleAfter from the snapshot's age budget.
func (s *Server) markStaleness(snap *usagestore.ProviderLimits) {
	if snap == nil {
		return
	}
	staleAfter := snap.SnapshotAt + int64(s.limitsStaleAge.Seconds())
	snap.StaleAfter = &staleAfter
}

// handleLimitsWatch records that the usage page is open, so limits are read
// on the watched interval for a while.
func (s *Server) handleLimitsWatch(w http.ResponseWriter, r *http.Request) {
	s.limitsWatch.MarkWatched()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProviderLimits(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	snap, err := s.store.LatestLimits(provider)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if snap == nil {
		http.Error(w, `{"error":"no snapshot"}`, http.StatusNotFound)
		return
	}
	s.markStaleness(snap)
	writeJSON(w, snap)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	f := usagestore.LimitHistoryFilter{
		Provider: r.PathValue("provider"),
		Window:   r.URL.Query().Get("window"),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Since = n
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Until = n
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	rows, err := s.store.HistoryLimits(f)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if rows == nil {
		rows = []usagestore.LimitHistoryRow{}
	}
	writeJSON(w, rows)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	switch provider {
	case "anthropic":
		snap, raw, err := s.anthropic.Fetch()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := s.store.SaveLimits(*snap, raw); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, snap)
	case "codex":
		if s.codex == nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("codex limits are off: usage_store.codex_command is not set or not found"))
			return
		}
		snap, raw, err := s.codex.Read(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := s.store.SaveLimits(*snap, raw); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, snap)
	default:
		http.Error(w, `{"error":"unknown provider"}`, http.StatusBadRequest)
	}
}

// ---- per-key spend + topups ----

// SpendKeysResponse is the per-provider, per-key shape returned by
// /api/usage/spend/keys. Each provider has an Account header (admin/parent
// row in the UI) plus per-key rows beneath. Window totals on KeyRow are
// computed from spend_daily at read time.
type SpendKeysResponse struct {
	Anthropic ProviderAccount `json:"anthropic"`
	OpenAI    ProviderAccount `json:"openai"`
}

type ProviderAccount struct {
	Configured         bool               `json:"configured"`
	AdminKeyHint       string             `json:"admin_key_hint"`
	Total24h           float64            `json:"total_usd_24h"`
	Total7d            float64            `json:"total_usd_7d"`
	Total30d           float64            `json:"total_usd_30d"`
	Topups             []usagestore.Topup `json:"topups"`
	TopupsTotalUSD     float64            `json:"topups_total_usd"`
	SpendSinceBaseline float64            `json:"spend_since_baseline"`
	RemainingUSD       *float64           `json:"remaining_usd"` // nil when no top-ups
	BalanceSince       *int64             `json:"balance_since"` // unix sec; nil when no top-ups
	Keys               []KeyRow           `json:"keys"`
}

type KeyRow struct {
	APIKeyID     string  `json:"api_key_id"`
	APIKeyName   string  `json:"api_key_name"`
	APIKeyHint   string  `json:"api_key_hint"`
	APIKeyStatus string  `json:"api_key_status"`
	Total24h     float64 `json:"total_usd_24h"`
	Total7d      float64 `json:"total_usd_7d"`
	Total30d     float64 `json:"total_usd_30d"`
	FetchedAt    int64   `json:"fetched_at"`
}

func (s *Server) handleSpendKeys(w http.ResponseWriter, r *http.Request) {
	resp := SpendKeysResponse{
		Anthropic: s.assembleProvider("anthropic", s.spendAnthropic != nil),
		OpenAI:    s.assembleProvider("openai", false),
	}
	writeJSON(w, resp)
}

// perKeyTotalsLoggingReadFailure reads one spend window and reports a failed
// read to the log, naming the provider and the window, before answering with
// whatever the store returned. The three windows used to discard their errors
// into `_`, so an unreadable spend_daily table rendered every figure as $0.00
// and printed nothing at all — a plausible answer for an account that has spent
// nothing, and the direction that costs the user money.
//
// It logs rather than propagates because assembleProvider has no error channel
// and handleSpendKeys always writes 200. Giving the endpoint one is a wire
// contract change and is the open half of card 0e5d003c; this is the half that
// was never in question.
func (s *Server) perKeyTotalsLoggingReadFailure(provider, window string, sinceUnix int64) map[string]float64 {
	totals, err := s.store.PerKeyTotals(provider, sinceUnix)
	if err != nil {
		log.Printf("[spend] per-key totals %s %s: %v", provider, window, err)
	}
	return totals
}

// assembleProvider builds one ProviderAccount: pulls KeyMeta + per-window
// spend totals + top-ups and combines them.
func (s *Server) assembleProvider(provider string, configured bool) ProviderAccount {
	out := ProviderAccount{
		Configured:   configured,
		AdminKeyHint: s.adminHint(provider),
		Topups:       []usagestore.Topup{},
		Keys:         []KeyRow{},
	}

	// Per-window totals: 24h / 7d / 30d. Cutoffs are UTC-day aligned to match
	// how spend_daily is keyed; "24h" is effectively today + yesterday.
	now := time.Now().UTC()
	day1 := now.AddDate(0, 0, -1).Truncate(24 * time.Hour).Unix()
	day7 := now.AddDate(0, 0, -7).Truncate(24 * time.Hour).Unix()
	day30 := now.AddDate(0, 0, -30).Truncate(24 * time.Hour).Unix()

	per24 := s.perKeyTotalsLoggingReadFailure(provider, "24h", day1)
	per7 := s.perKeyTotalsLoggingReadFailure(provider, "7d", day7)
	per30 := s.perKeyTotalsLoggingReadFailure(provider, "30d", day30)

	metas, err := s.store.ListKeyMeta(provider)
	if err != nil {
		log.Printf("[spend] list meta %s: %v", provider, err)
	}
	for _, m := range metas {
		row := KeyRow{
			APIKeyID:     m.APIKeyID,
			APIKeyName:   m.APIKeyName,
			APIKeyHint:   m.APIKeyHint,
			APIKeyStatus: m.APIKeyStatus,
			Total24h:     per24[m.APIKeyID],
			Total7d:      per7[m.APIKeyID],
			Total30d:     per30[m.APIKeyID],
			FetchedAt:    m.FetchedAt,
		}
		out.Keys = append(out.Keys, row)
		out.Total24h += row.Total24h
		out.Total7d += row.Total7d
		out.Total30d += row.Total30d
	}

	// Top-ups + remaining computation. "Remaining" = sum(top-ups) − spend
	// since the earliest top-up date. If no top-ups, remaining is nil so the
	// UI knows to show the "Add credit" empty state instead of $0.
	topups, err := s.store.ListTopups(provider)
	if err != nil {
		log.Printf("[spend] list topups %s: %v", provider, err)
	}
	if topups != nil {
		out.Topups = topups
	}
	if len(out.Topups) > 0 {
		var topupsTotal float64
		for _, t := range out.Topups {
			topupsTotal += t.AmountUSD
		}
		baseline := out.Topups[0].OccurredAt
		spend, err := s.store.SumSpendSince(provider, "", baseline)
		if err != nil {
			log.Printf("[spend] sum since baseline %s: %v", provider, err)
		}
		remaining := topupsTotal - spend
		out.TopupsTotalUSD = topupsTotal
		out.SpendSinceBaseline = spend
		out.RemainingUSD = &remaining
		out.BalanceSince = &baseline
	}

	return out
}

func (s *Server) handleSpendKeyRaw(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	apiKeyID := r.PathValue("api_key_id")
	raw, err := s.store.LatestKeyRaw(provider, apiKeyID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if raw == "" {
		http.Error(w, `{"error":"no snapshot for that key"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(raw))
}

func (s *Server) handleSpendRefresh(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	switch provider {
	case "anthropic":
		if s.spendAnthropic == nil {
			http.Error(w, `{"error":"anthropic admin credential not configured"}`, http.StatusNotFound)
			return
		}
		res, err := s.spendAnthropic.Fetch()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := s.persistAnthropic(res); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		s.SaveAdminHint("anthropic", res.AdminKeyHint)
		writeJSON(w, map[string]interface{}{"keys": len(res.Keys), "daily": len(res.Daily)})
	default:
		http.Error(w, `{"error":"unknown provider"}`, http.StatusBadRequest)
	}
}

// persistAnthropic writes one Fetch result into the store.
func (s *Server) persistAnthropic(res *spend.AnthropicResult) error {
	for _, m := range res.Keys {
		if err := s.store.SaveKeyMeta(m); err != nil {
			return fmt.Errorf("save key meta %s: %w", m.APIKeyID, err)
		}
	}
	for _, d := range res.Daily {
		if err := s.store.SaveDailySpend(d); err != nil {
			return fmt.Errorf("save daily %s/%s: %w", d.APIKeyID, d.Date, err)
		}
	}
	return nil
}

// ---- topups ----

func (s *Server) handleListTopups(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		http.Error(w, `{"error":"provider query param required"}`, http.StatusBadRequest)
		return
	}
	topups, err := s.store.ListTopups(provider)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if topups == nil {
		topups = []usagestore.Topup{}
	}
	writeJSON(w, topups)
}

type topupRequest struct {
	Provider      string  `json:"provider"`
	AmountUSD     float64 `json:"amount_usd"`
	OccurredAt    int64   `json:"occurred_at"`     // unix sec, optional → defaults to now
	OccurredAtStr string  `json:"occurred_at_str"` // YYYY-MM-DD, optional → parsed UTC midnight
	Note          string  `json:"note"`
}

func (s *Server) handleAddTopup(w http.ResponseWriter, r *http.Request) {
	var req topupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	occurredAt := req.OccurredAt
	if occurredAt == 0 && req.OccurredAtStr != "" {
		t, err := time.Parse("2006-01-02", req.OccurredAtStr)
		if err != nil {
			// Bounded: occurred_at_str is caller-supplied, this service has no
			// auth and listens on every interface, and time.Parse quotes its
			// whole input back inside its own error twice. Measured against the
			// deployed binary, a 5 000-byte value rendered a 10 096-byte body.
			writeErr(w, http.StatusBadRequest, fmt.Errorf("occurred_at_str: %w", boundedtext.Error(err)))
			return
		}
		occurredAt = t.UTC().Unix()
	}
	if occurredAt == 0 {
		occurredAt = time.Now().Unix()
	}
	id, err := s.store.AddTopup(usagestore.Topup{
		Provider:   req.Provider,
		AmountUSD:  req.AmountUSD,
		OccurredAt: occurredAt,
		Note:       req.Note,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (s *Server) handleDeleteTopup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, `{"error":"bad id"}`, http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteTopup(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- misc ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
