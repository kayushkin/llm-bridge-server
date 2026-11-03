package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestMachineStatsSamples(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	percent := func(value float64) *float64 { return &value }
	for minute := 0; minute < 3; minute++ {
		for _, machineID := range []string{"m_laptop", "m_server"} {
			stats := msg.MachineStats{CollectedAt: base.Add(time.Duration(minute) * time.Minute), CPUUsagePercent: percent(float64(minute))}
			if err := s.RecordMachineStatsSample(machineID, stats); err != nil {
				t.Fatal(err)
			}
		}
	}

	latest, err := s.LatestMachineStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 || *latest["m_laptop"].CPUUsagePercent != 2 || !latest["m_server"].CollectedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("latest %+v", latest)
	}

	samples, err := s.MachineStatsSamplesBetween("m_laptop", base.Add(time.Minute), base.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || *samples[0].CPUUsagePercent != 1 {
		t.Fatalf("between %+v", samples)
	}

	deleted, err := s.DeleteMachineStatsSamplesBefore(base.Add(time.Minute))
	if err != nil || deleted != 2 {
		t.Fatalf("deleted %d %v", deleted, err)
	}

	if err := s.RecordMachineStatsSample("", msg.MachineStats{CollectedAt: base}); err == nil {
		t.Fatal("a sample with no machine id must be refused")
	}
	if err := s.RecordMachineStatsSample("m_laptop", msg.MachineStats{}); err == nil {
		t.Fatal("a sample with no time must be refused")
	}
}
