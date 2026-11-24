package childprocess

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// processIsRunning reports whether pid names a process that has not exited.
// Signal 0 cannot answer this: it succeeds on a zombie, which has exited and
// only waits for its parent to collect its status. /proc/<pid>/stat carries the
// state letter after the parenthesised command name; Z is a zombie and X a
// process being torn down.
func processIsRunning(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		panic("read process state: " + err.Error())
	}
	afterName := stat[strings.LastIndexByte(string(stat), ')')+1:]
	fields := strings.Fields(string(afterName))
	if len(fields) == 0 {
		panic("unparseable /proc/" + strconv.Itoa(pid) + "/stat: " + string(stat))
	}
	state := fields[0]
	return state != "Z" && state != "X"
}

// A dead process that has not been reaped must read as exited, or a
// grandchild killed on time is reported as having survived whenever its
// adoptive parent is slow to reap it.
func TestProcessIsRunningTreatsAnUnreapedProcessAsExited(t *testing.T) {
	process, err := os.StartProcess("/bin/sleep", []string{"sleep", "60"}, &os.ProcAttr{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := process.Pid
	if !processIsRunning(pid) {
		t.Fatalf("a sleeping process %d read as exited", pid)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// Not reaped yet: the process stays a zombie until Wait below.
	if alive := waitForProcessToExit(pid, 5*time.Second); alive {
		_, _ = process.Wait()
		t.Fatalf("killed, unreaped process %d read as still running", pid)
	}
	if _, err := process.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if processIsRunning(pid) {
		t.Fatalf("reaped process %d read as running", pid)
	}
}
