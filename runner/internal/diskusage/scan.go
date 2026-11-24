package diskusage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/kayushkin/llm-bridge/msg"
)

// Scanner walks a directory tree without following links.
type Scanner struct {
	Keep KeepPolicy
	// Parallelism is how many directories it reads at once. Reading several
	// at once is much faster on a solid-state drive.
	Parallelism int
	// StayOnDevice keeps the scan on the root's file system: a directory on
	// another device (Windows' drives under /mnt in WSL, /proc) becomes a
	// mount_point entry and is not read.
	StayOnDevice bool
	// CountDiskBlocks counts each file by the space it takes on disk rather
	// than its length, and a file with several hard links once. Inside WSL a
	// scan by length found 796 GB where Linux said 552 GB was in use
	// (2026-11-24): sparse files and hard links.
	CountDiskBlocks bool

	rootDevice uint64
	linksMu    sync.Mutex
	linksSeen  map[[2]uint64]bool

	slots        chan struct{}
	entriesSeen  atomic.Int64
	bytesCounted atomic.Uint64
}

// Progress is how many entries the scan has seen and how many bytes of
// files it has counted so far. Safe to call while Scan runs.
func (s *Scanner) Progress() (int64, uint64) {
	return s.entriesSeen.Load(), s.bytesCounted.Load()
}

// Scan walks the directory at root and returns its tree, named name.
func (s *Scanner) Scan(root, name string) (*Node, error) {
	if !s.Keep.valid() || s.Parallelism < 1 {
		return nil, fmt.Errorf("scanner needs a keep policy and a parallelism of at least 1")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if kind, _ := classify(info); kind != msg.DiskUsageEntryDirectory {
		return nil, fmt.Errorf("%s is a %s, not a directory", root, kind)
	}
	if s.StayOnDevice {
		device, ok := deviceOf(info)
		if !ok {
			return nil, fmt.Errorf("cannot tell which device %s is on, so the scan could not stay on it", root)
		}
		s.rootDevice = device
	}
	s.linksSeen = map[[2]uint64]bool{}
	s.slots = make(chan struct{}, s.Parallelism-1)
	return s.directory(root, name, info), nil
}

func (s *Scanner) directory(path, name string, info fs.FileInfo) *Node {
	node := &Node{Name: name, Kind: msg.DiskUsageEntryDirectory, ModifiedAtUnixSeconds: info.ModTime().Unix()}
	entries, err := os.ReadDir(path)
	if err != nil && len(entries) == 0 {
		node.finishDirectory(nil, s.Keep, true)
		return node
	}
	unreadable := err != nil // read part of it
	children := make([]*Node, len(entries))
	var wait sync.WaitGroup
	for i, entry := range entries {
		s.entriesSeen.Add(1)
		entryInfo, err := entry.Info()
		if err != nil {
			continue // it went away between listing and reading
		}
		kind, cloudOnly := classify(entryInfo)
		if kind == msg.DiskUsageEntryDirectory && s.StayOnDevice {
			if device, ok := deviceOf(entryInfo); !ok || device != s.rootDevice {
				kind = msg.DiskUsageEntryMountPoint
			}
		}
		switch kind {
		case msg.DiskUsageEntryDirectory:
			childPath := filepath.Join(path, entry.Name())
			select {
			case s.slots <- struct{}{}:
				wait.Add(1)
				go func(i int, childPath, childName string, childInfo fs.FileInfo) {
					defer func() { <-s.slots; wait.Done() }()
					children[i] = s.directory(childPath, childName, childInfo)
				}(i, childPath, entry.Name(), entryInfo)
			default:
				children[i] = s.directory(childPath, entry.Name(), entryInfo)
			}
		case msg.DiskUsageEntryFile:
			file := &Node{Name: entry.Name(), Kind: kind, ModifiedAtUnixSeconds: entryInfo.ModTime().Unix()}
			switch {
			case cloudOnly:
				file.CloudOnlyFileCount = 1
			case s.CountDiskBlocks:
				file.SizeBytes = s.diskBytesCountedOnce(entryInfo)
			default:
				file.SizeBytes = uint64(entryInfo.Size())
			}
			s.bytesCounted.Add(file.SizeBytes)
			children[i] = file
		default:
			children[i] = &Node{Name: entry.Name(), Kind: kind, ModifiedAtUnixSeconds: entryInfo.ModTime().Unix()}
		}
	}
	wait.Wait()
	node.finishDirectory(children, s.Keep, unreadable)
	return node
}

// diskBytesCountedOnce is the space a file takes on disk, or 0 for a hard
// link to a file the scan has already counted.
func (s *Scanner) diskBytesCountedOnce(info fs.FileInfo) uint64 {
	bytes, linkKey, linkCount, ok := fileDiskBytes(info)
	if !ok {
		return uint64(info.Size())
	}
	if linkCount > 1 {
		s.linksMu.Lock()
		seen := s.linksSeen[linkKey]
		s.linksSeen[linkKey] = true
		s.linksMu.Unlock()
		if seen {
			return 0
		}
	}
	return bytes
}
