//go:build !linux

package main

import "fmt"

// filesystemSize is read only in WSL, which is Linux.
func filesystemSize(path string) (total, free, used uint64, err error) {
	return 0, 0, 0, fmt.Errorf("the WSL file system is measured only on Linux")
}
