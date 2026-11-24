package limitsrefresh

import (
	"context"
	"errors"
	"testing"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

var testIntervals = Intervals{Idle: 15 * time.Minute, Watched: 2 * time.Minute, WatcherExpiry: 3 * time.Minute}

type fakeProvider struct {
	reads  int
	result error
}

func (f *fakeProvider) fetch(context.Context) (*usagestore.ProviderLimits, []byte, error) {
	f.reads++
	if f.result != nil {
		return nil, nil, f.result
	}
	return &usagestore.ProviderLimits{Provider: "fake"}, nil, nil
}

func newTestRefresher(provider *fakeProvider, clock *time.Time) *Refresher {
	refresher := New(testIntervals,
		func(usagestore.ProviderLimits, []byte) error { return nil },
		[]Provider{{Name: "fake", Fetch: provider.fetch}})
	refresher.now = func() time.Time { return *clock }
	return refresher
}

// readsOverMinutes steps the clock one minute at a time and checks every step.
func readsOverMinutes(refresher *Refresher, clock *time.Time, minutes int, eachMinute func()) {
	for minute := 0; minute < minutes; minute++ {
		if eachMinute != nil {
			eachMinute()
		}
		refresher.refreshDueProviders(context.Background())
		*clock = clock.Add(time.Minute)
	}
}

func TestIdleReadsOnlyEveryIdleInterval(t *testing.T) {
	clock := time.Unix(0, 0)
	provider := &fakeProvider{}
	refresher := newTestRefresher(provider, &clock)
	readsOverMinutes(refresher, &clock, 60, nil)
	// minutes 0, 15, 30, 45
	if provider.reads != 4 {
		t.Fatalf("reads in an idle hour = %d, want 4", provider.reads)
	}
}

func TestWatchedReadsEveryWatchedInterval(t *testing.T) {
	clock := time.Unix(0, 0)
	provider := &fakeProvider{}
	refresher := newTestRefresher(provider, &clock)
	readsOverMinutes(refresher, &clock, 20, refresher.MarkWatched)
	// minutes 0, 2, 4, … 18
	if provider.reads != 10 {
		t.Fatalf("reads in 20 watched minutes = %d, want 10", provider.reads)
	}
}

func TestOpeningThePageReadsAtOnceWhenAWatchedReadIsDue(t *testing.T) {
	clock := time.Unix(0, 0)
	provider := &fakeProvider{}
	refresher := newTestRefresher(provider, &clock)
	refresher.refreshDueProviders(context.Background())
	clock = clock.Add(5 * time.Minute)
	refresher.refreshDueProviders(context.Background())
	if provider.reads != 1 {
		t.Fatalf("idle read five minutes after the last one; reads = %d", provider.reads)
	}
	refresher.MarkWatched()
	refresher.refreshDueProviders(context.Background())
	if provider.reads != 2 {
		t.Fatalf("opening the page did not read; reads = %d", provider.reads)
	}
}

func TestWatchingExpiresWithoutHeartbeats(t *testing.T) {
	clock := time.Unix(0, 0)
	refresher := newTestRefresher(&fakeProvider{}, &clock)
	refresher.MarkWatched()
	clock = clock.Add(testIntervals.WatcherExpiry - time.Second)
	if !refresher.Watched() {
		t.Fatal("watching ended before the expiry")
	}
	clock = clock.Add(2 * time.Second)
	if refresher.Watched() {
		t.Fatal("watching outlived the expiry")
	}
}

func TestFailuresBackOffWhileWatchedAndASuccessClearsIt(t *testing.T) {
	clock := time.Unix(0, 0)
	provider := &fakeProvider{result: errors.New("429")}
	refresher := newTestRefresher(provider, &clock)
	var readMinutes []int
	for minute := 0; minute < 60; minute++ {
		refresher.MarkWatched()
		before := provider.reads
		refresher.refreshDueProviders(context.Background())
		if provider.reads != before {
			readMinutes = append(readMinutes, minute)
		}
		clock = clock.Add(time.Minute)
	}
	// waits after each failure: 2, 4, 8, then capped at 15
	want := []int{0, 2, 6, 14, 29, 44, 59}
	if len(readMinutes) != len(want) {
		t.Fatalf("read at minutes %v, want %v", readMinutes, want)
	}
	for i := range want {
		if readMinutes[i] != want[i] {
			t.Fatalf("read at minutes %v, want %v", readMinutes, want)
		}
	}

	provider.result = nil
	clock = clock.Add(15 * time.Minute)
	refresher.MarkWatched()
	refresher.refreshDueProviders(context.Background())
	clock = clock.Add(testIntervals.Watched)
	refresher.MarkWatched()
	before := provider.reads
	refresher.refreshDueProviders(context.Background())
	if provider.reads != before+1 {
		t.Fatal("a success did not restore the watched interval")
	}
}
