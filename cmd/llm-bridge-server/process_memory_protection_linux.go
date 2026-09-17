package main

import (
	"fmt"
	"syscall"
)

// forbidSameUserProcessesFromReadingThisProcess marks this process
// non-dumpable. Scrubbing secrets out of child environments
// (internal/childprocessenv) is not enough on its own: an agent runs as the
// same Unix user as this server, and a same-user process can otherwise read
// /proc/<server pid>/environ — which still holds the signing key and tokens as
// the process was started with them — or ptrace this process and read its
// memory. With PR_SET_DUMPABLE=0 the kernel refuses both to anyone without
// CAP_SYS_PTRACE. It also disables core dumps. Children are unaffected:
// dumpable is reset on execve.
func forbidSameUserProcessesFromReadingThisProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", errno)
	}
	return nil
}
