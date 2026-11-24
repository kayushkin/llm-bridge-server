package usagestore

import "testing"

func windowMinutes(minutes int64) *int64 { return &minutes }

func TestLatestLimitsOfAProviderNeverReadIsNil(t *testing.T) {
	s := newTestStore(t)
	latest, err := s.LatestLimits("codex")
	if err != nil {
		t.Fatalf("LatestLimits: %v", err)
	}
	if latest != nil {
		t.Fatalf("a provider with no snapshot has no latest limits, got %+v", latest)
	}
}

func TestLatestLimitsRebuildsTheNewestSnapshotAcrossItsWindows(t *testing.T) {
	s := newTestStore(t)
	older := ProviderLimits{Provider: "anthropic", PlanType: "max", Tier: "tier-a", SnapshotAt: 100, Source: "api",
		Windows: map[string]*LimitWindow{"five_hour": {UsedPercent: 10}, "seven_day": {UsedPercent: 20}}}
	newer := ProviderLimits{Provider: "anthropic", PlanType: "max", Tier: "tier-b", SnapshotAt: 200, Source: "api",
		Windows: map[string]*LimitWindow{"five_hour": {UsedPercent: 30, WindowMinutes: windowMinutes(300)}, "seven_day": {UsedPercent: 40}}}
	for _, snapshot := range []ProviderLimits{older, newer} {
		if err := s.SaveLimits(snapshot, []byte(`{"raw":true}`)); err != nil {
			t.Fatalf("SaveLimits: %v", err)
		}
	}
	if err := s.SaveLimits(ProviderLimits{Provider: "codex", SnapshotAt: 300, Source: "app-server",
		Windows: map[string]*LimitWindow{"weekly": {UsedPercent: 99}}}, nil); err != nil {
		t.Fatalf("SaveLimits codex: %v", err)
	}

	latest, err := s.LatestLimits("anthropic")
	if err != nil {
		t.Fatalf("LatestLimits: %v", err)
	}
	if latest.SnapshotAt != 200 || latest.Tier != "tier-b" || len(latest.Windows) != 2 {
		t.Fatalf("want the snapshot at 200 with two windows, got %+v", latest)
	}
	if latest.Windows["five_hour"].UsedPercent != 30 || *latest.Windows["five_hour"].WindowMinutes != 300 {
		t.Fatalf("five_hour window: %+v", latest.Windows["five_hour"])
	}
	if latest.Windows["seven_day"].WindowMinutes != nil {
		t.Fatalf("a window saved with no length must come back with none: %+v", latest.Windows["seven_day"])
	}
}

func TestHistoryLimitsFiltersByWindowAndTimeAndKeepsTheLimit(t *testing.T) {
	s := newTestStore(t)
	for at := int64(1); at <= 5; at++ {
		if err := s.SaveLimits(ProviderLimits{Provider: "anthropic", SnapshotAt: at * 100, Source: "api",
			Windows: map[string]*LimitWindow{"five_hour": {UsedPercent: float64(at)}, "seven_day": {UsedPercent: float64(at * 10)}}}, nil); err != nil {
			t.Fatalf("SaveLimits: %v", err)
		}
	}
	rows, err := s.HistoryLimits(LimitHistoryFilter{Provider: "anthropic", Window: "seven_day", Since: 200, Until: 400, Limit: 2})
	if err != nil {
		t.Fatalf("HistoryLimits: %v", err)
	}
	if len(rows) != 2 || rows[0].SnapshotAt != 200 || rows[1].SnapshotAt != 300 || rows[0].UsedPercent != 20 {
		t.Fatalf("want seven_day at 200 and 300, oldest first, got %+v", rows)
	}
	if _, err := s.HistoryLimits(LimitHistoryFilter{}); err == nil {
		t.Fatal("a history with no provider must be refused")
	}
}
