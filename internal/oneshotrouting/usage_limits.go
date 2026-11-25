package oneshotrouting

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// UsageStoreLimits reads usage-store's limits, the same answer as its
// GET /api/usage/limits. One answer is kept for cacheFor, so a burst of calls
// asks usage-store once; usage-store itself refreshes a provider no more often
// than every two minutes.
type UsageStoreLimits struct {
	read     UsageLimitsReader
	cacheFor time.Duration
	now      func() time.Time

	mutex    sync.Mutex
	cached   map[string]UsageProviderLimits
	cachedAt time.Time
}

// UsageProviderLimits is one provider's latest snapshot, as usage-store
// answers it.
type UsageProviderLimits struct {
	// UsedPercentByWindow is each window's use, by window name.
	UsedPercentByWindow map[string]float64
	// StaleAfter is the unix second after which the snapshot says nothing
	// about now. Zero is a snapshot with no age budget, which says nothing.
	StaleAfter int64
}

// UsageLimitsReader answers every provider usage-store reports, by
// usage-store's provider name. A usage-store that cannot answer is an error.
type UsageLimitsReader func(ctx context.Context) (map[string]UsageProviderLimits, error)

// NewUsageStoreLimits reads usage-store's limits through read. The server
// gives it usage-store in this process.
func NewUsageStoreLimits(read UsageLimitsReader) *UsageStoreLimits {
	return &UsageStoreLimits{
		read:     read,
		cacheFor: 30 * time.Second,
		now:      time.Now,
	}
}

// ExhaustedWindow implements LimitReader: the first window of usageProvider at
// 100% or more, when its snapshot is not stale. A provider usage-store does not
// report is not exhausted.
func (limits *UsageStoreLimits) ExhaustedWindow(ctx context.Context, usageProvider string) (string, error) {
	providers, err := limits.readKept(ctx)
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
	names := make([]string, 0, len(provider.UsedPercentByWindow))
	for name := range provider.UsedPercentByWindow {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if used := provider.UsedPercentByWindow[name]; used >= 100 {
			return fmt.Sprintf("%s limit at %.0f%%", name, used), nil
		}
	}
	return "", nil
}

func (limits *UsageStoreLimits) readKept(ctx context.Context) (map[string]UsageProviderLimits, error) {
	limits.mutex.Lock()
	defer limits.mutex.Unlock()
	if limits.cached != nil && limits.now().Sub(limits.cachedAt) < limits.cacheFor {
		return limits.cached, nil
	}
	providers, err := limits.read(ctx)
	if err != nil {
		return nil, fmt.Errorf("usage-store: %w", err)
	}
	limits.cached, limits.cachedAt = providers, limits.now()
	return providers, nil
}
