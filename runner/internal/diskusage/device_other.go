//go:build !windows

package diskusage

import (
	"io/fs"
	"syscall"
)

// deviceOf is the device the entry lives on, from its lstat.
func deviceOf(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Dev), true
}

// fileDiskBytes is the space a file takes on disk, which for a sparse file
// is less than its length, and whether this is one of several hard links to
// the same file, keyed by device and inode.
func fileDiskBytes(info fs.FileInfo) (bytes uint64, linkKey [2]uint64, linkCount uint64, ok bool) {
	stat, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, [2]uint64{}, 0, false
	}
	return uint64(stat.Blocks) * 512, [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}, uint64(stat.Nlink), true
}
