//go:build unix && !linux

package childprocess

import "syscall"

// processIsRunning reports whether pid names a process that has not been
// reaped. Without /proc there is no portable way to tell a zombie from a
// running process, so an unreaped zombie reads as running here.
func processIsRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
