package evaluations

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ModelCatalog says what model-store knows about each model.
type ModelCatalog interface {
	Models(ctx context.Context) (map[string]ModelPrice, error)
}

// UsageReader reads subscription usage from usage-store.
type UsageReader interface {
	Limits(ctx context.Context) (UsageLimits, error)
}

// UsageLimits is usage-store's latest limits, the answer of its
// GET /api/usage/limits: per subscription, its windows and when the reading
// goes stale.
type UsageLimits map[string]UsageSubscription

// UsageSubscription is one subscription's reading.
type UsageSubscription struct {
	Windows    map[string]UsageWindow `json:"windows"`
	SnapshotAt int64                  `json:"snapshot_at"`
	StaleAfter int64                  `json:"stale_after"`
}

// UsageWindow is one window's use.
type UsageWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

// budgetVerdict checks a reading against a run's limits. It returns the
// reason to stop, or "" to go on. A reading that is missing or stale stops
// the run too: a guard that cannot see is no guard.
func budgetVerdict(limits UsageLimits, budget []BudgetLimit, now time.Time) string {
	var reasons []string
	for _, limit := range budget {
		subscription, ok := limits[limit.Subscription]
		if !ok {
			reasons = append(reasons, fmt.Sprintf("usage-store has no reading for %s", limit.Subscription))
			continue
		}
		if subscription.StaleAfter != 0 && now.Unix() > subscription.StaleAfter {
			reasons = append(reasons, fmt.Sprintf("usage-store's reading for %s went stale at %s", limit.Subscription,
				time.Unix(subscription.StaleAfter, 0).UTC().Format(time.RFC3339)))
			continue
		}
		window, ok := subscription.Windows[limit.Window]
		if !ok {
			reasons = append(reasons, fmt.Sprintf("usage-store has no %s window for %s", limit.Window, limit.Subscription))
			continue
		}
		if window.UsedPercent > limit.MaxUsedPercent {
			reasons = append(reasons, fmt.Sprintf("%s %s is at %g%%, above this run's limit of %g%%", limit.Subscription, limit.Window,
				window.UsedPercent, limit.MaxUsedPercent))
		}
	}
	return strings.Join(reasons, "; ")
}

// ParseBudgetLimits reads the evaluations.default_budget_limits setting's
// subscription.window:percent pairs, in a stable order.
func ParseBudgetLimits(pairs map[string]string) ([]BudgetLimit, error) {
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	limits := make([]BudgetLimit, 0, len(keys))
	for _, key := range keys {
		subscription, window, found := strings.Cut(key, ".")
		percent, err := strconv.ParseFloat(pairs[key], 64)
		if !found || subscription == "" || window == "" || err != nil || percent <= 0 || percent > 100 {
			return nil, fmt.Errorf("%s:%s is not subscription.window:percent with a percent in (0, 100]", key, pairs[key])
		}
		limits = append(limits, BudgetLimit{Subscription: subscription, Window: window, MaxUsedPercent: percent})
	}
	return limits, nil
}
