//go:build linux

package main

import "syscall"

// filesystemSize is the size of the file system holding path, the space on
// it free to this user, and the space files take. Used is not total less
// free: ext4 keeps about 5% back for root, which is in neither (54 GB of a
// 1 TB WSL disk, 2026-11-24), and counting it as used understated what
// compacting frees.
func filesystemSize(path string) (total, free, used uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0, err
	}
	blockSize := uint64(stat.Bsize)
	return stat.Blocks * blockSize, stat.Bavail * blockSize, (stat.Blocks - stat.Bfree) * blockSize, nil
}
