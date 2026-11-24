//go:build !windows

package diskusage

import (
	"errors"

	"github.com/kayushkin/llm-bridge/msg"
)

var errOnlyOnWindows = errors.New("the disk-usage helper scans and deletes only on Windows")

// CheckDeletion refuses everything off Windows.
func CheckDeletion(path string, expectedKind msg.DiskUsageEntryKind) Verdict {
	return refuse("%v", errOnlyOnWindows)
}

// RequireFixedDrive refuses every drive off Windows.
func RequireFixedDrive(drive string) error { return errOnlyOnWindows }

// Drives lists nothing off Windows.
func Drives() ([]msg.DiskUsageDrive, error) { return nil, errOnlyOnWindows }
