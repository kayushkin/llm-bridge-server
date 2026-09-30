package oneshotrouting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// UsageStoreLimits reads usage-store's GET /api/usage/limits. One answer is
// kept for cacheFor, so a burst of calls asks usage-store once; usage-store
// itself refreshes a provider no more often than every two minutes.
type UsageStoreLimits struct {
	baseURL  string
	client   *http.Client
	cacheFor time.Duration
	now      func() time.Time

	mutex    sync.Mutex
	cached   map[string]usageStoreProviderLimits
	cachedAt time.Time
}

// usageStoreProviderLimits is one provider in usage-store's answer.
type usageStoreProviderLimits struct {
	Windows map[string]struct {
		UsedPercent float64 `json:"used_percent"`
	} `json:"windows"`
	// StaleAfter is the unix second after which the snapshot says nothing
	// about now.
	StaleAfter int64 `json:"stale_after"`
}

// NewUsageStoreLimits reads usage-store at baseURL.
func NewUsageStoreLimits(baseURL string) *UsageStoreLimits {
	return &UsageStoreLimits{
		baseURL:  strings.TrimRight(baseURL, "/"),
		client:   &http.Client{Timeout: 3 * time.Second},
		cacheFor: 30 * time.Second,
		now:      time.Now,
	}
}

// ExhaustedWindow implements LimitReader: the first window of usageProvider at
// 100% or more, when its snapshot is not stale. A provider usage-store does not
// report is not exhausted.
func (limits *UsageStoreLimits) ExhaustedWindow(ctx context.Context, usageProvider string) (string, error) {
	providers, err := limits.read(ctx)
	if err != nil {
		return "", err
	}
	provider, reported := providers[usageProvider]
	if !reported {
		return "", nil
	}
	if provider.StaleAfter == 0 || limits.now().Unix() > provider.StaleAfter {
		return "", nil
	}
	names := make([]string, 0, len(provider.Windows))
	for name := range provider.Windows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if used := provider.Windows[name].UsedPercent; used >= 100 {
			return fmt.Sprintf("%s limit at %.0f%%", name, used), nil
		}
	}
	return "", nil
}

func (limits *UsageStoreLimits) read(ctx context.Context) (map[string]usageStoreProviderLimits, error) {
	limits.mutex.Lock()
	defer limits.mutex.Unlock()
	if limits.cached != nil && limits.now().Sub(limits.cachedAt) < limits.cacheFor {
		return limits.cached, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, limits.baseURL+"/api/usage/limits", nil)
	if err != nil {
		return nil, err
	}
	response, err := limits.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("usage-store: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("usage-store: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var providers map[string]usageStoreProviderLimits
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("usage-store limits are not what this reads: %w", err)
	}
	limits.cached, limits.cachedAt = providers, limits.now()
	return providers, nil
}
