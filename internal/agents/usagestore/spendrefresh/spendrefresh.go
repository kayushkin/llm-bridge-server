// Package spendrefresh finds the Anthropic admin key in auth-store and reads
// what each API key cost, on a long interval, into usage-store.
package spendrefresh

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	usagestore "github.com/kayushkin/llm-bridge-server/internal/agents/usagestore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/usagestore/spend"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

// AuthStore is the part of auth-store the discovery reads.
type AuthStore interface {
	List(ctx context.Context, filter authstoreclient.ListFilter) ([]authstoreclient.Credential, error)
	Resolve(ctx context.Context, id, reason string) (*authstoreclient.Resolved, error)
}

// anthropicAdminKeyPrefix is how an Anthropic admin key begins
// ("sk-ant-admin01-…", with no dash after "admin").
const anthropicAdminKeyPrefix = "sk-ant-admin"

// DiscoverAnthropicCollector resolves each Anthropic api_key credential in
// auth-store until one holds an admin key, and returns a collector that
// resolves that credential again on every fetch, so a rotated key is picked up.
// Every resolve shows in auth-store's audit trail on purpose. It returns nil
// and no error when auth-store holds no admin key.
func DiscoverAnthropicCollector(ctx context.Context, authStore AuthStore) (*spend.AnthropicKeyCollector, error) {
	credentials, err := authStore.List(ctx, authstoreclient.ListFilter{Provider: "anthropic"})
	if err != nil {
		return nil, fmt.Errorf("listing auth-store's credentials: %w", err)
	}
	adminCredentialID := ""
	for _, credential := range credentials {
		if credential.Provider != "anthropic" || credential.AuthType != "api_key" {
			continue
		}
		resolved, err := authStore.Resolve(ctx, credential.ID, "discover:admin-key")
		if err != nil {
			log.Printf("[spend] resolve %s for discovery failed: %v", credential.ID, err)
			continue
		}
		if strings.HasPrefix(resolved.APIKey, anthropicAdminKeyPrefix) {
			adminCredentialID = credential.ID
			break
		}
	}
	if adminCredentialID == "" {
		return nil, nil
	}
	log.Printf("[spend] anthropic admin credential discovered: %s", adminCredentialID)
	collector := spend.NewAnthropicKey(func() (string, error) {
		resolved, err := authStore.Resolve(context.Background(), adminCredentialID, "fetch:anthropic-spend-per-key")
		if err != nil {
			return "", err
		}
		if resolved.APIKey == "" {
			return "", fmt.Errorf("credential %s has no api_key (auth_type=%s)", adminCredentialID, resolved.AuthType)
		}
		return resolved.APIKey, nil
	})
	collector.OnUnknownModel = func(model string) {
		log.Printf("[spend] unknown anthropic model %q (priced as $0; add to spend/pricing.go)", model)
	}
	return collector, nil
}

// AdminHintRecorder keeps the admin key hint of the latest fetch for the
// usage page.
type AdminHintRecorder interface {
	SaveAdminHint(provider, hint string)
}

// Run reads per-key spend now and then every interval until ctx ends. A failed
// fetch or write is logged and the next one tried.
func Run(ctx context.Context, store *usagestore.Store, hints AdminHintRecorder, collector *spend.AnthropicKeyCollector, interval time.Duration) {
	refresh := func() {
		result, err := collector.Fetch()
		if err != nil {
			log.Printf("[spend-refresh] anthropic: %v", err)
			return
		}
		for _, key := range result.Keys {
			if err := store.SaveKeyMeta(key); err != nil {
				log.Printf("[spend-refresh] save meta %s: %v", key.APIKeyID, err)
			}
		}
		for _, day := range result.Daily {
			if err := store.SaveDailySpend(day); err != nil {
				log.Printf("[spend-refresh] save daily %s/%s: %v", day.APIKeyID, day.Date, err)
			}
		}
		hints.SaveAdminHint("anthropic", result.AdminKeyHint)
	}
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
