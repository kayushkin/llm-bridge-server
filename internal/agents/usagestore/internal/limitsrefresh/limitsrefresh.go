// Package limitsrefresh decides when usage-store asks each provider for its
// subscription limits.
//
// Asking costs something: Anthropic's usage endpoint rate-limits the same login
// every Claude Code process on the host uses, and a Codex read starts a CLI
// process. So the refresher asks rarely by default (the idle interval) and often
// only while someone is looking at the usage page (the watched interval). The
// page says it is being looked at by calling MarkWatched about once a minute
// while it is visible; a mark older than the watcher expiry no longer counts.
//
// A failed read pushes the next one back: the first failure waits one watched
// interval, each further failure doubles the wait, never past the idle interval.
// A success clears it. Retrying a rate limit at full speed only prolongs it.
package limitsrefresh

import (
	"context"
	"log"
	"sync"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

// FetchLimits reads one provider's current limits and the raw response.
type FetchLimits func(ctx context.Context) (*usagestore.ProviderLimits, []byte, error)

// SaveLimits persists one snapshot.
type SaveLimits func(limits usagestore.ProviderLimits, raw []byte) error

// Provider is one source of limits the refresher polls.
type Provider struct {
	Name  string
	Fetch FetchLimits
}

// Intervals are the refresher's timing rules.
type Intervals struct {
	Idle          time.Duration // between reads while nobody watches
	Watched       time.Duration // between reads while the usage page is open
	WatcherExpiry time.Duration // how long one MarkWatched keeps the watched interval
}

type providerState struct {
	provider       Provider
	lastAttemptAt  time.Time // zero until the first attempt
	failureBackoff time.Duration
}

// Refresher polls every provider on the intervals above.
type Refresher struct {
	intervals Intervals
	save      SaveLimits
	now       func() time.Time

	mutex         sync.Mutex
	lastWatchedAt time.Time
	wasWatched    bool
	providers     []*providerState

	wake chan struct{}
}

// New returns a Refresher over providers. Call Run to start it.
func New(intervals Intervals, save SaveLimits, providers []Provider) *Refresher {
	refresher := &Refresher{
		intervals: intervals,
		save:      save,
		now:       time.Now,
		wake:      make(chan struct{}, 1),
	}
	for _, provider := range providers {
		refresher.providers = append(refresher.providers, &providerState{provider: provider})
	}
	return refresher
}

// MarkWatched records that someone is looking at the limits now. If that turns
// the watched interval on, a read that is now due happens at once instead of on
// the next scheduled check.
func (r *Refresher) MarkWatched() {
	r.mutex.Lock()
	r.lastWatchedAt = r.now()
	r.mutex.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Watched reports whether a MarkWatched is recent enough to count.
func (r *Refresher) Watched() bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.watchedLocked()
}

func (r *Refresher) watchedLocked() bool {
	return !r.lastWatchedAt.IsZero() && r.now().Sub(r.lastWatchedAt) < r.intervals.WatcherExpiry
}

// Run reads every due provider, then checks again at least every checkEvery or
// whenever MarkWatched is called, until ctx ends.
func (r *Refresher) Run(ctx context.Context, checkEvery time.Duration) {
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		r.refreshDueProviders(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

// refreshDueProviders reads each provider whose next read is due.
func (r *Refresher) refreshDueProviders(ctx context.Context) {
	for _, state := range r.dueProviders() {
		limits, raw, err := state.provider.Fetch(ctx)
		if err == nil {
			err = r.save(*limits, raw)
		}
		r.recordAttempt(state, err)
	}
}

func (r *Refresher) dueProviders() []*providerState {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	watched := r.watchedLocked()
	if watched != r.wasWatched {
		if watched {
			log.Printf("[limits] usage page open: reading limits every %s", r.intervals.Watched)
		} else {
			log.Printf("[limits] usage page closed: reading limits every %s", r.intervals.Idle)
		}
		r.wasWatched = watched
	}
	now := r.now()
	var due []*providerState
	for _, state := range r.providers {
		if state.lastAttemptAt.IsZero() || !now.Before(state.lastAttemptAt.Add(r.waitLocked(state, watched))) {
			due = append(due, state)
		}
	}
	return due
}

// waitLocked is how long after its last attempt a provider is read again.
func (r *Refresher) waitLocked(state *providerState, watched bool) time.Duration {
	wait := r.intervals.Idle
	if watched {
		wait = r.intervals.Watched
	}
	if state.failureBackoff > wait {
		wait = state.failureBackoff
	}
	return wait
}

func (r *Refresher) recordAttempt(state *providerState, err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	state.lastAttemptAt = r.now()
	if err == nil {
		state.failureBackoff = 0
		return
	}
	switch {
	case state.failureBackoff == 0:
		state.failureBackoff = r.intervals.Watched
	case state.failureBackoff*2 > r.intervals.Idle:
		state.failureBackoff = r.intervals.Idle
	default:
		state.failureBackoff *= 2
	}
	log.Printf("[limits] %s: %v (next attempt in at least %s)", state.provider.Name, err, state.failureBackoff)
}
