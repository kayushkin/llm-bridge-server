package executor_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
)

// The exact shape that defeated the old 5-minute timeout: the direct child
// exits immediately, but a grandchild survives holding the stdout pipe open.
// The grandchild heartbeats to a file, so we can tell whether it truly died
// rather than just whether Run returned.
func TestTimeoutKillsGrandchildHoldingPipe(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "beat")
	cmd := fmt.Sprintf("sh -c 'while true; do date +%%s%%N > %s; sleep 0.1; done' & echo spawned; exit 0", beat)

	start := time.Now()
	res := executor.Run(cmd, 2*time.Second)
	elapsed := time.Since(start)
	t.Logf("status=%q elapsed=%v", res.Status, elapsed)

	if elapsed > 20*time.Second {
		t.Fatalf("Run blocked %v — grandchild held the pipe open", elapsed)
	}

	// If the grandchild is dead, the heartbeat file stops changing.
	read := func() string { b, _ := os.ReadFile(beat); return string(b) }
	first := read()
	time.Sleep(700 * time.Millisecond)
	if second := read(); second != first {
		t.Fatalf("grandchild still heartbeating after the run — leaked to PID 1 (%q -> %q)", first, second)
	}
	t.Logf("grandchild confirmed dead (heartbeat frozen at %q)", strings.TrimSpace(first))
}

func TestTimeoutFiresOnSlowCommand(t *testing.T) {
	start := time.Now()
	res := executor.Run("sleep 60", 2*time.Second)
	elapsed := time.Since(start)
	if res.Status != "timeout" {
		t.Fatalf("want status=timeout, got %q (%v)", res.Status, elapsed)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout took %v, want ~2s", elapsed)
	}
	t.Logf("OK: status=%q after %v", res.Status, elapsed)
}

// A job that finishes inside its timeout must be unaffected by the group kill.
func TestFastCommandStillSucceeds(t *testing.T) {
	res := executor.Run("echo hello", 5*time.Second)
	if res.Status != "success" || res.Output != "hello\n" {
		t.Fatalf("want success/hello, got %q / %q", res.Status, res.Output)
	}
}

func TestNonZeroExitIsError(t *testing.T) {
	res := executor.Run("echo boom; exit 3", 5*time.Second)
	if res.Status != "error" || !strings.Contains(res.Output, "boom") {
		t.Fatalf("want error containing boom, got %q / %q", res.Status, res.Output)
	}
}

// A long timeout must be honored, not clipped to the old global 5 minutes.
func TestLongTimeoutIsHonored(t *testing.T) {
	res := executor.Run("sleep 1", 2*time.Hour)
	if res.Status != "success" {
		t.Fatalf("want success, got %q / %q", res.Status, res.Output)
	}
}
