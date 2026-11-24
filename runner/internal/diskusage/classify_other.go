//go:build !windows

package diskusage

import (
	"io/fs"

	"github.com/kayushkin/llm-bridge/msg"
)

// classify says what an entry is, without following it. Off Windows it is
// used by tests: a symbolic link is a link, and anything that is neither a
// directory nor a regular file counts as an empty file.
func classify(info fs.FileInfo) (kind msg.DiskUsageEntryKind, cloudOnly bool) {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return msg.DiskUsageEntryLink, false
	case mode.IsDir():
		return msg.DiskUsageEntryDirectory, false
	}
	return msg.DiskUsageEntryFile, false
}
