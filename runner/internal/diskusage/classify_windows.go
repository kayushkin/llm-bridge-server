//go:build windows

package diskusage

import (
	"io/fs"
	"syscall"

	"github.com/kayushkin/llm-bridge/msg"
)

// Attributes Windows sets on a file whose data is not on this machine: a
// OneDrive "online-only" file and the like. Reading one downloads it.
const cloudOnlyAttributes = 0x1000 /* OFFLINE */ | 0x40000 /* RECALL_ON_OPEN */ | 0x400000 /* RECALL_ON_DATA_ACCESS */

// classify says what an entry is, without following it.
//
// Go reports a symbolic link as ModeSymlink and a junction as a
// non-directory that still carries FILE_ATTRIBUTE_DIRECTORY (Go 1.23 and
// later). Both are links: the scan neither follows nor counts them, and a
// deletion removes the link, never what it points at. Other reparse points,
// such as a OneDrive folder, are real directories and report ModeDir.
func classify(info fs.FileInfo) (kind msg.DiskUsageEntryKind, cloudOnly bool) {
	var attributes uint32
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		attributes = data.FileAttributes
	}
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return msg.DiskUsageEntryLink, false
	case mode.IsDir():
		return msg.DiskUsageEntryDirectory, false
	case attributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0:
		return msg.DiskUsageEntryLink, false
	}
	return msg.DiskUsageEntryFile, attributes&cloudOnlyAttributes != 0
}
