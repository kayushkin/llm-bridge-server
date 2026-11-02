package harness

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestSSHReachabilityIsProbedOnceWhileFresh puts a fake ssh first on PATH
// that counts its runs, and asks for one machine's reachability from many
// callers at once and then again: one probe answers them all until its answer
// is older than machineReachabilityFreshFor.
func TestSSHReachabilityIsProbedOnceWhileFresh(t *testing.T) {
	directory := t.TempDir()
	countFile := filepath.Join(directory, "runs")
	script := "#!/bin/sh\necho run >> " + countFile + "\nsleep 0.2\necho ok\n"
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	runs := func() int {
		data, _ := os.ReadFile(countFile)
		return strings.Count(string(data), "run")
	}

	manager := &Manager{}
	machine := &msg.Machine{ID: "m_test", Name: "test", Transport: msg.TransportSSH, Hostname: "test.invalid"}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reachable, _ := manager.ReachabilityOfMachine(machine); !reachable {
				t.Error("the fake ssh answered ok, but the machine reads as unreachable")
			}
		}()
	}
	wg.Wait()
	_, checkedAt := manager.ReachabilityOfMachine(machine)
	if got := runs(); got != 1 {
		t.Fatalf("six asks while fresh ran ssh %d times, want once", got)
	}
	if time.Since(checkedAt) > 5*time.Second {
		t.Fatalf("checkedAt = %v, want when the probe ran", checkedAt)
	}

	saved := machineReachabilityFreshFor
	machineReachabilityFreshFor = 0
	t.Cleanup(func() { machineReachabilityFreshFor = saved })
	manager.ReachabilityOfMachine(machine)
	if got := runs(); got != 2 {
		t.Fatalf("an ask after the answer went stale ran ssh %d times in all, want twice", got)
	}
}
