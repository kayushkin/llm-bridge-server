package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	usagestore "github.com/kayushkin/usage-store"
	"github.com/kayushkin/usage-store/anthropic"
	"github.com/kayushkin/usage-store/codex"
	"github.com/kayushkin/usage-store/internal/authstore"
	"github.com/kayushkin/usage-store/internal/config"
	"github.com/kayushkin/usage-store/internal/limitsrefresh"
	"github.com/kayushkin/usage-store/internal/server"
	"github.com/kayushkin/usage-store/spend"
)

func main() {
	settings, err := config.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("[usage-store] read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("[usage-store] read settings: %v", err)
	}
	cfg, err := config.Load(settings)
	if err != nil {
		log.Fatalf("[usage-store] read settings: %v", err)
	}

	store, err := usagestore.Open(cfg.LimitsDBPath)
	if err != nil {
		log.Fatalf("[usage-store] open store: %v", err)
	}
	defer store.Close()

	ant := anthropic.New()
	// A nil reader turns codex limits off; the rest of the service still runs.
	var cx *codex.Reader
	if cfg.CodexCommand == "" {
		log.Printf("[usage-store] codex limits OFF: USAGE_STORE_CODEX_COMMAND is not set")
	} else if cx, err = codex.New(cfg.CodexCommand, cfg.CodexTimeout); err != nil {
		log.Printf("[usage-store] codex limits OFF: %v", err)
		cx = nil
	}

	sc := buildSpendCollectors(cfg)

	limitsProviders := []limitsrefresh.Provider{{
		Name:  "anthropic",
		Fetch: func(context.Context) (*usagestore.ProviderLimits, []byte, error) { return ant.Fetch() },
	}}
	if cx != nil {
		limitsProviders = append(limitsProviders, limitsrefresh.Provider{Name: "codex", Fetch: cx.Read})
	}
	limitsRefresher := limitsrefresh.New(limitsrefresh.Intervals{
		Idle:          cfg.LimitsIdleRefreshInterval,
		Watched:       cfg.LimitsWatchedRefreshInterval,
		WatcherExpiry: cfg.LimitsWatcherExpiry,
	}, store.SaveLimits, limitsProviders)
	limitsStaleAge := time.Duration(cfg.LimitsStaleAfterIdleIntervals) * cfg.LimitsIdleRefreshInterval

	srv := server.New(store, cfg.TokensDBPath, ant, cx, limitsRefresher, limitsStaleAge, sc, settings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go limitsRefresher.Run(ctx, 15*time.Second)
	go runSpendRefresher(ctx, store, srv, sc, cfg.SpendRefreshInterval)

	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: srv}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[usage-store] shutdown signal received")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		httpSrv.Shutdown(shutdownCtx)
		cancel()
	}()

	log.Printf("[usage-store] listening on %s (limits db: %s, tokens db: %s, limits refresh: %s idle / %s watched, spend refresh: %s, anthropic-admin: %s, codex: %s)",
		cfg.ListenAddr, cfg.LimitsDBPath, cfg.TokensDBPath, cfg.LimitsIdleRefreshInterval, cfg.LimitsWatchedRefreshInterval, cfg.SpendRefreshInterval,
		boolStr(sc.Anthropic != nil), boolStr(cx != nil))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[usage-store] server error: %v", err)
	}
}

// buildSpendCollectors discovers admin credentials in auth-store rather than
// taking explicit credential IDs. It scans every credential, resolves the
// api_key, and matches by prefix (sk-ant-admin- for Anthropic, sk-admin- for
// OpenAI). The first match wins. Future rotations are picked up because the
// resolve happens lazily on every Fetch.
func buildSpendCollectors(cfg config.Config) server.SpendCollectors {
	out := server.SpendCollectors{}

	if cfg.AuthStoreToken == "" {
		log.Printf("[spend] AUTH_STORE_TOKEN not set; spend collection disabled")
		return out
	}
	asClient := authstore.New(cfg.AuthStoreURL, cfg.AuthStoreToken, "usage-store")

	creds, err := asClient.ListCredentials()
	if err != nil {
		log.Printf("[spend] auth-store list failed: %v", err)
		return out
	}

	// Anthropic admin keys are formatted "sk-ant-admin01-..." (no dash after
	// "admin"); OpenAI uses "sk-admin-...". Match the common prefix.
	anthropicAdminID := findAdminCredID(asClient, creds, "anthropic", "sk-ant-admin")
	if anthropicAdminID != "" {
		log.Printf("[spend] anthropic admin credential discovered: %s", anthropicAdminID)
		out.Anthropic = spend.NewAnthropicKey(func() (string, error) {
			r, err := asClient.ResolveByID(anthropicAdminID, "fetch:anthropic-spend-per-key")
			if err != nil {
				return "", err
			}
			return r.APIKey, nil
		})
		out.Anthropic.OnUnknownModel = func(model string) {
			log.Printf("[spend] unknown anthropic model %q (priced as $0; add to spend/pricing.go)", model)
		}
	}
	return out
}

// findAdminCredID resolves each credential of the given provider until one
// returns an api_key with the expected admin prefix. Auditing every resolve
// is intentional — auth-store's audit trail will show this discovery sweep.
func findAdminCredID(c *authstore.Client, creds []authstore.CredentialSummary, provider, prefix string) string {
	for _, cred := range creds {
		if cred.Provider != provider || cred.AuthType != "api_key" {
			continue
		}
		r, err := c.ResolveByID(cred.ID, "discover:admin-key")
		if err != nil {
			log.Printf("[spend] resolve %s for discovery failed: %v", cred.ID, err)
			continue
		}
		if strings.HasPrefix(r.APIKey, prefix) {
			return cred.ID
		}
	}
	return ""
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// runSpendRefresher polls each configured provider's per-key spend on a
// longer cadence (default 1h). Failures are logged but never stop the loop.
// Admin-key hints are forwarded to the server so /spend/keys can render the
// parent row even before the UI's first /spend/refresh round-trip.
func runSpendRefresher(ctx context.Context, s *usagestore.Store, srv *server.Server, sc server.SpendCollectors, interval time.Duration) {
	if sc.Anthropic == nil {
		return
	}
	tick := func() {
		res, err := sc.Anthropic.Fetch()
		if err != nil {
			log.Printf("[spend-refresh] anthropic: %v", err)
			return
		}
		for _, m := range res.Keys {
			if err := s.SaveKeyMeta(m); err != nil {
				log.Printf("[spend-refresh] save meta %s: %v", m.APIKeyID, err)
			}
		}
		for _, d := range res.Daily {
			if err := s.SaveDailySpend(d); err != nil {
				log.Printf("[spend-refresh] save daily %s/%s: %v", d.APIKeyID, d.Date, err)
			}
		}
		srv.SaveAdminHint("anthropic", res.AdminKeyHint)
	}

	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
