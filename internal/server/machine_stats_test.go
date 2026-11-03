package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestBucketMachineStats(t *testing.T) {
	since := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	percent := func(value float64) *float64 { return &value }
	samples := []msg.MachineStats{
		{CollectedAt: since.Add(10 * time.Second), CPUUsagePercent: percent(10), MemoryUsedBytes: 1},
		{CollectedAt: since.Add(70 * time.Second), CPUUsagePercent: percent(90), MemoryUsedBytes: 2},
		{CollectedAt: since.Add(130 * time.Second), MemoryUsedBytes: 3},
		{CollectedAt: since.Add(400 * time.Second), CPUUsagePercent: percent(50), MemoryUsedBytes: 4},
	}
	points := bucketMachineStats(samples, since, 3*time.Minute)
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2 (the empty bucket left out)", len(points))
	}
	first := points[0]
	if !first.At.Equal(since) || first.SampleCount != 3 || *first.CPUUsagePercentAverage != 50 || *first.CPUUsagePercentMaximum != 90 || first.Stats.MemoryUsedBytes != 3 {
		t.Fatalf("first point %+v", first)
	}
	if second := points[1]; !second.At.Equal(since.Add(6*time.Minute)) || second.SampleCount != 1 || *second.CPUUsagePercentAverage != 50 {
		t.Fatalf("second point %+v", second)
	}
	if len(bucketMachineStats(nil, since, time.Minute)) != 0 {
		t.Fatal("no samples, no points")
	}
}

func TestParseMachineStatsHistoryQuery(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	since, until, step, err := parseMachineStatsHistoryQuery(httptest.NewRequest("GET", "/machines/m_1/stats/history", nil), now)
	if err != nil || !until.Equal(now) || !since.Equal(now.Add(-24*time.Hour)) || step != time.Minute {
		t.Fatalf("defaults: %v %v %v %v", since, until, step, err)
	}
	for _, query := range []string{
		"?step_seconds=30",
		"?since=2026-11-03T13:00:00Z",
		"?since=yesterday",
		"?since=2026-10-01T00:00:00Z&step_seconds=60",
	} {
		if _, _, _, err := parseMachineStatsHistoryQuery(httptest.NewRequest("GET", "/x"+query, nil), now); err == nil {
			t.Errorf("%s: expected an error", query)
		}
	}
	if _, _, step, err := parseMachineStatsHistoryQuery(httptest.NewRequest("GET", "/x?since=2026-10-04T12:00:00Z&step_seconds=3600", nil), now); err != nil || step != time.Hour {
		t.Fatalf("30 days hourly: %v %v", step, err)
	}
}
