//go:build !linux

package main

import (
	"fmt"
	"runtime"
)

// forbidSameUserProcessesFromReadingThisProcess has no implementation off
// Linux, and says so: this server must not start where a same-user agent could
// read the login signing key out of this process.
func forbidSameUserProcessesFromReadingThisProcess() error {
	return fmt.Errorf("no way to stop same-user processes reading this process's environment and memory on %s", runtime.GOOS)
}
