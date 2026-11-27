package operationstore_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

// The coordinator's hot path is one claim and a few receipt updates per
// operation, and an empty claim from every idle worker once a second. This
// benchmark times each of those calls one by one over a store that already
// holds about as many finished operations as this host's, and reports the
// median and the 99th percentile in microseconds. It runs a fixed number of
// rounds whatever b.N is, so run it with -benchtime 1x.

const (
	benchmarkFinishedOperations = 2000
	benchmarkRounds             = 2000
)

func BenchmarkCoordinatorClaimAndUpdate(b *testing.B) {
	store := openBenchmarkStore(b)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tick := func() time.Time { now = now.Add(time.Millisecond); return now }

	runOne := func(key string, claims, progressUpdates, finishingUpdates *[]time.Duration) {
		intent := msg.OperationIntent{Type: "llm.completion", OrganizationID: "principal_000006", PrincipalID: "principal_000004",
			IdempotencyKey: key, Input: []byte(`{"model":"test-model","prompt":"a prompt of ordinary length for a completion"}`)}
		if _, _, err := store.Create(intent, "fingerprint-"+key, tick()); err != nil {
			b.Fatal(err)
		}
		started := time.Now()
		operation, found, err := store.Claim("benchmark-owner", time.Minute, tick())
		elapsed := time.Since(started)
		if err != nil || !found {
			b.Fatalf("claim: found=%v err=%v", found, err)
		}
		if claims != nil {
			*claims = append(*claims, elapsed)
		}
		started = time.Now()
		if _, err := store.Update(operation.Receipt.ID, operation.LeaseToken, tick(), func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
			stored.Receipt.Progress = &msg.OperationProgress{Message: "halfway", Completed: 1, Total: 2}
			return msg.OperationEventProgress, nil
		}); err != nil {
			b.Fatal(err)
		}
		elapsed = time.Since(started)
		if progressUpdates != nil {
			*progressUpdates = append(*progressUpdates, elapsed)
		}
		started = time.Now()
		if _, err := store.Update(operation.Receipt.ID, operation.LeaseToken, tick(), func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
			stored.Receipt.State = msg.OperationStateSucceeded
			stored.Receipt.Result = []byte(`{"text":"an answer of ordinary length from the model"}`)
			return msg.OperationEventSucceeded, nil
		}); err != nil {
			b.Fatal(err)
		}
		elapsed = time.Since(started)
		if finishingUpdates != nil {
			*finishingUpdates = append(*finishingUpdates, elapsed)
		}
	}

	for index := 0; index < benchmarkFinishedOperations; index++ {
		runOne(fmt.Sprintf("seed-%d", index), nil, nil, nil)
	}

	var claims, progressUpdates, finishingUpdates, emptyClaims []time.Duration
	b.ResetTimer()
	for round := 0; round < benchmarkRounds; round++ {
		runOne(fmt.Sprintf("round-%d", round), &claims, &progressUpdates, &finishingUpdates)
		started := time.Now()
		if _, found, err := store.Claim("benchmark-owner", time.Minute, tick()); err != nil || found {
			b.Fatalf("empty claim: found=%v err=%v", found, err)
		}
		emptyClaims = append(emptyClaims, time.Since(started))
	}
	b.StopTimer()
	reportPercentiles(b, "claim", claims)
	reportPercentiles(b, "progress-update", progressUpdates)
	reportPercentiles(b, "finishing-update", finishingUpdates)
	reportPercentiles(b, "empty-claim", emptyClaims)
}

func reportPercentiles(b *testing.B, name string, durations []time.Duration) {
	sorted := append([]time.Duration{}, durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(fraction float64) float64 {
		return float64(sorted[int(fraction*float64(len(sorted)-1))].Microseconds())
	}
	b.ReportMetric(at(0.50), name+"-p50-µs")
	b.ReportMetric(at(0.99), name+"-p99-µs")
}
