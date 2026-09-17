package main

import (
	"syscall"
	"testing"
)

func TestForbidSameUserProcessesFromReadingThisProcessClearsDumpable(t *testing.T) {
	if err := forbidSameUserProcessesFromReadingThisProcess(); err != nil {
		t.Fatal(err)
	}
	dumpable, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if errno != 0 {
		t.Fatalf("prctl(PR_GET_DUMPABLE): %v", errno)
	}
	if dumpable != 0 {
		t.Errorf("dumpable = %d after the call, want 0: /proc/<pid>/environ stays readable by same-user processes", dumpable)
	}
}
