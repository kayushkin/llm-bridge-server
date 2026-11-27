package harness

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

type timedEvents struct {
	sessionlog.Store
	mu    sync.Mutex
	spent map[string]time.Duration
	count map[string]int
}

func (t *timedEvents) note(name string, start time.Time) {
	t.mu.Lock()
	t.spent[name] += time.Since(start)
	t.count[name]++
	t.mu.Unlock()
}
func (t *timedEvents) StoreSessionEvent(r store.SessionEventRecord) (int64, error) {
	defer t.note("store "+r.Type, time.Now())
	return t.Store.StoreSessionEvent(r)
}
func (t *timedEvents) WriteSessionStatus(id string, ev *msg.Event) (int64, error) {
	defer t.note("status", time.Now())
	return t.Store.WriteSessionStatus(id, ev)
}

func BenchmarkPumpProbe(b *testing.B) {
	for _, path := range benchmarkSessionEventPaths[:2] {
		bench := path.build(b)
		timed := &timedEvents{Store: bench.manager.SessionEvents(), spent: map[string]time.Duration{}, count: map[string]int{}}
		bench.manager.SetSessionEventStore(timed)
		ingest, send := &latencies{}, &latencies{}
		driver := startSessionDriver(b, bench.manager, bench.bridgeStore, "br_probe", 1, ingest, send)
		start := time.Now()
		driver.runTurns(b, bench.manager, 4, false)
		elapsed := time.Since(start)
		driver.stop(bench.manager)
		var total time.Duration
		for name, d := range timed.spent {
			total += d
			fmt.Printf("%s %s: n=%d total=%.0fms mean=%.2fms\n", path.name, name, timed.count[name], milliseconds(d), milliseconds(d)/float64(timed.count[name]))
		}
		fmt.Printf("%s: frames=%d elapsed=%.0fms in-store=%.0fms ingest p50=%.2f\n", path.name, len(ingest.values), milliseconds(elapsed), milliseconds(total), milliseconds(ingest.quantile(0.5)))
	}
}
