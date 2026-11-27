package machines

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// GET /instances reads every instance and every machine; most other routes and
// every session start read one instance by id. This benchmark times those
// reads one by one over about as many machines and instances as this host
// has, and reports the median and the 99th percentile in microseconds. It runs
// a fixed number of rounds whatever b.N is, so run it with -benchtime 1x.
func BenchmarkInstanceReads(b *testing.B) {
	store := openBenchmarkStore(b)
	var instanceIDs []string
	for machineIndex := 0; machineIndex < 5; machineIndex++ {
		machine := &msg.Machine{ID: fmt.Sprintf("m_bench_%d", machineIndex), Name: fmt.Sprintf("bench-%d", machineIndex),
			Hostname: "localhost", Transport: msg.TransportRunner, OS: "linux", Arch: "amd64"}
		if err := store.CreateMachine(machine); err != nil {
			b.Fatal(err)
		}
		for instanceIndex := 0; instanceIndex < 4; instanceIndex++ {
			instance := &msg.Instance{ID: fmt.Sprintf("inst_bench_%d_%d", machineIndex, instanceIndex), HarnessType: msg.HarnessClaudeCode,
				Name: fmt.Sprintf("bench-%d-%d", machineIndex, instanceIndex), MachineID: machine.ID, WorkingDir: "/work", Enabled: true}
			if err := store.CreateInstance(instance); err != nil {
				b.Fatal(err)
			}
			instanceIDs = append(instanceIDs, instance.ID)
		}
	}

	const rounds = 5000
	var lists, gets, getsWithMachine []time.Duration
	b.ResetTimer()
	for round := 0; round < rounds; round++ {
		started := time.Now()
		instances, err := store.ListInstances()
		if err != nil || len(instances) != len(instanceIDs) {
			b.Fatalf("list instances: %d, %v", len(instances), err)
		}
		if _, err := store.ListMachines(); err != nil {
			b.Fatal(err)
		}
		lists = append(lists, time.Since(started))

		id := instanceIDs[round%len(instanceIDs)]
		started = time.Now()
		if _, err := store.GetInstance(id); err != nil {
			b.Fatal(err)
		}
		gets = append(gets, time.Since(started))

		started = time.Now()
		if _, err := store.GetInstanceWithMachine(id); err != nil {
			b.Fatal(err)
		}
		getsWithMachine = append(getsWithMachine, time.Since(started))
	}
	b.StopTimer()
	reportPercentiles(b, "list-instances-and-machines", lists)
	reportPercentiles(b, "get-instance", gets)
	reportPercentiles(b, "get-instance-with-machine", getsWithMachine)
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
