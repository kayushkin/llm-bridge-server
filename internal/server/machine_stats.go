package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/kayushkin/llm-bridge/machinestats"
	"github.com/kayushkin/llm-bridge/msg"
)

// Machine stats: every runner sends a reading of its machine once a minute
// (llm-bridge-runner), and this server reads its own machine on the same
// beat. Each reading is kept for machineStatsRetention and served as the
// newest per machine (GET /machines/stats) and as history
// (GET /machines/{id}/stats/history), which the Machines page draws and
// healthcheck alerts on.

// machineStatsRetention is how long readings are kept.
const machineStatsRetention = 30 * 24 * time.Hour

// machineStatsPruneInterval is how often readings past retention are deleted.
const machineStatsPruneInterval = time.Hour

// machineStatsHistoryMaximumPoints bounds one history answer.
const machineStatsHistoryMaximumPoints = 5000

// StartMachineStatsSampler files runners' readings, reads this server's own
// machine every minute, and deletes readings past retention.
func (s *Server) StartMachineStatsSampler(ctx context.Context) {
	if s.harnessStore == nil {
		log.Printf("[machine-stats] no harness store; machine stats are off")
		return
	}
	s.harness.Runners().SetMachineStatsReceiver(s.recordMachineStats)
	collector := machinestats.NewCollector()
	go func() {
		ticker := time.NewTicker(machinestats.SampleInterval)
		defer ticker.Stop()
		var lastPrunedAt time.Time
		for {
			s.sampleLocalMachines(ctx, collector)
			if time.Since(lastPrunedAt) >= machineStatsPruneInterval {
				s.pruneMachineStats()
				lastPrunedAt = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// recordMachineStats stores one reading.
func (s *Server) recordMachineStats(machineID string, stats msg.MachineStats) {
	if err := s.store.RecordMachineStatsSample(machineID, stats); err != nil {
		log.Printf("[machine-stats] record reading of %s: %v", machineID, err)
	}
}

// sampleLocalMachines reads this host and files the reading under every
// machine whose transport is local: each of them is this host.
func (s *Server) sampleLocalMachines(ctx context.Context, collector *machinestats.Collector) {
	machines, err := s.harnessStore.ListMachines()
	if err != nil {
		log.Printf("[machine-stats] list machines: %v", err)
		return
	}
	var localMachineIDs []string
	for _, machine := range machines {
		if machine.Transport == msg.TransportLocal {
			localMachineIDs = append(localMachineIDs, machine.ID)
		}
	}
	if len(localMachineIDs) == 0 {
		return
	}
	stats := collector.Collect(ctx)
	for _, machineID := range localMachineIDs {
		s.recordMachineStats(machineID, stats)
	}
}

func (s *Server) pruneMachineStats() {
	deleted, err := s.store.DeleteMachineStatsSamplesBefore(time.Now().Add(-machineStatsRetention))
	if err != nil {
		log.Printf("[machine-stats] delete readings past retention: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[machine-stats] deleted %d readings older than %s", deleted, machineStatsRetention)
	}
}

// handleMachineStatsOverview serves GET /machines/stats: every machine with
// its newest reading, sorted by name.
func (s *Server) handleMachineStatsOverview(w http.ResponseWriter, r *http.Request) {
	machines, err := s.harnessStore.ListMachines()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	latest, err := s.store.LatestMachineStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runners := s.harness.Runners()
	overviews := make([]msg.MachineStatsOverview, 0, len(machines))
	for _, machine := range machines {
		overview := msg.MachineStatsOverview{Machine: machine}
		if machine.Transport == msg.TransportRunner {
			connected := runners.Get(machine.Name) != nil
			overview.RunnerConnected = &connected
		}
		if stats, found := latest[machine.ID]; found {
			overview.Latest = &stats
		}
		overviews = append(overviews, overview)
	}
	sort.Slice(overviews, func(i, j int) bool { return overviews[i].Machine.Name < overviews[j].Machine.Name })
	writeJSON(w, overviews)
}

// handleMachineStatsHistory serves GET /machines/{id}/stats/history.
// Query: since and until (RFC 3339; default the last 24 hours) and
// step_seconds (default 60, at least 60).
func (s *Server) handleMachineStatsHistory(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("id")
	if _, err := s.harnessStore.GetMachine(machineID); err != nil {
		http.Error(w, "machine not found", http.StatusNotFound)
		return
	}
	since, until, step, err := parseMachineStatsHistoryQuery(r, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	samples, err := s.store.MachineStatsSamplesBetween(machineID, since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, msg.MachineStatsHistory{
		MachineID:   machineID,
		Since:       since,
		Until:       until,
		StepSeconds: int64(step / time.Second),
		Points:      bucketMachineStats(samples, since, step),
	})
}

// parseMachineStatsHistoryQuery reads and checks the history query.
func parseMachineStatsHistoryQuery(r *http.Request, now time.Time) (since, until time.Time, step time.Duration, err error) {
	query := r.URL.Query()
	until = now
	if value := query.Get("until"); value != "" {
		if until, err = time.Parse(time.RFC3339, value); err != nil {
			return since, until, step, fmt.Errorf("until: %w", err)
		}
	}
	since = until.Add(-24 * time.Hour)
	if value := query.Get("since"); value != "" {
		if since, err = time.Parse(time.RFC3339, value); err != nil {
			return since, until, step, fmt.Errorf("since: %w", err)
		}
	}
	if !since.Before(until) {
		return since, until, step, fmt.Errorf("since must be before until")
	}
	step = machinestats.SampleInterval
	if value := query.Get("step_seconds"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return since, until, step, fmt.Errorf("step_seconds: %w", err)
		}
		step = time.Duration(seconds) * time.Second
		if step < machinestats.SampleInterval {
			return since, until, step, fmt.Errorf("step_seconds must be at least %d", int64(machinestats.SampleInterval/time.Second))
		}
	}
	if points := until.Sub(since) / step; points > machineStatsHistoryMaximumPoints {
		return since, until, step, fmt.Errorf("%d points is more than %d; use a larger step_seconds", points, machineStatsHistoryMaximumPoints)
	}
	return since.UTC(), until.UTC(), step, nil
}

// bucketMachineStats groups readings, oldest first, into buckets of step
// starting at since. Each point carries the bucket's newest reading and its
// CPU mean and peak. Empty buckets are left out.
func bucketMachineStats(samples []msg.MachineStats, since time.Time, step time.Duration) []msg.MachineStatsHistoryPoint {
	points := []msg.MachineStatsHistoryPoint{}
	var cpuSum float64
	var cpuCount int
	finish := func() {
		if cpuCount == 0 {
			return
		}
		point := &points[len(points)-1]
		point.CPUUsagePercentAverage = new(float64)
		*point.CPUUsagePercentAverage = cpuSum / float64(cpuCount)
	}
	for _, sample := range samples {
		bucketStart := since.Add(sample.CollectedAt.Sub(since) / step * step)
		if len(points) == 0 || !points[len(points)-1].At.Equal(bucketStart) {
			finish()
			points = append(points, msg.MachineStatsHistoryPoint{At: bucketStart})
			cpuSum, cpuCount = 0, 0
		}
		point := &points[len(points)-1]
		point.SampleCount++
		point.Stats = sample
		if sample.CPUUsagePercent != nil {
			cpuSum += *sample.CPUUsagePercent
			cpuCount++
			if point.CPUUsagePercentMaximum == nil || *sample.CPUUsagePercent > *point.CPUUsagePercentMaximum {
				maximum := *sample.CPUUsagePercent
				point.CPUUsagePercentMaximum = &maximum
			}
		}
	}
	finish()
	return points
}
