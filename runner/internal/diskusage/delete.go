package diskusage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/kayushkin/llm-bridge/msg"
)

// maximumReportedErrors bounds the errors a deletion keeps to report.
const maximumReportedErrors = 20

// Deletion deletes a file or a directory tree for good. It never follows a
// link: a link inside the tree is removed as a link, and what it points at
// is left alone. It carries on past a file it cannot delete (one a program
// has open, say) and reports what it could not.
type Deletion struct {
	// StayOnDevice leaves alone any directory on another device than the
	// deleted path's, such as a Windows drive mounted inside WSL.
	StayOnDevice bool
	// CountDiskBlocks counts what a deleted file freed by the space it took
	// on disk, and nothing for a link to a file that still has others.
	CountDiskBlocks bool

	rootDevice       uint64
	mu               sync.Mutex
	deletedBytes     uint64
	deletedFileCount int64
	failedEntryCount int64
	errors           []string
}

// DeletionProgress is what a deletion has done so far.
type DeletionProgress struct {
	DeletedBytes     uint64   `json:"deleted_bytes"`
	DeletedFileCount int64    `json:"deleted_file_count"`
	FailedEntryCount int64    `json:"failed_entry_count"`
	Errors           []string `json:"errors,omitempty"`
}

// Progress is what the deletion has done so far. Safe to call while Run runs.
func (d *Deletion) Progress() DeletionProgress {
	d.mu.Lock()
	defer d.mu.Unlock()
	return DeletionProgress{
		DeletedBytes:     d.deletedBytes,
		DeletedFileCount: d.deletedFileCount,
		FailedEntryCount: d.failedEntryCount,
		Errors:           append([]string(nil), d.errors...),
	}
}

// Run deletes path, which the caller has checked is a file or a directory
// of kind expectedKind and no link.
func (d *Deletion) Run(path string, expectedKind msg.DiskUsageEntryKind) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	kind, _ := classify(info)
	if kind != expectedKind {
		return fmt.Errorf("%s is now a %s, not a %s; nothing was deleted", path, kind, expectedKind)
	}
	if d.StayOnDevice {
		device, ok := deviceOf(info)
		if !ok {
			return fmt.Errorf("cannot tell which device %s is on; nothing was deleted", path)
		}
		d.rootDevice = device
	}
	d.remove(path, info)
	return nil
}

func (d *Deletion) remove(path string, info fs.FileInfo) {
	kind, cloudOnly := classify(info)
	switch kind {
	case msg.DiskUsageEntryDirectory:
		if d.StayOnDevice {
			if device, ok := deviceOf(info); !ok || device != d.rootDevice {
				d.failed(path, fmt.Errorf("another file system is mounted here; it was left alone"))
				return
			}
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			d.failed(path, err)
		}
		for _, entry := range entries {
			entryInfo, err := entry.Info()
			if err != nil {
				if !os.IsNotExist(err) {
					d.failed(filepath.Join(path, entry.Name()), err)
				}
				continue
			}
			d.remove(filepath.Join(path, entry.Name()), entryInfo)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			d.failed(path, err)
		}
	case msg.DiskUsageEntryLink:
		// os.Remove deletes a link or junction itself, never its target.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			d.failed(path, err)
		}
	default:
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				d.failed(path, err)
			}
			return
		}
		freed := uint64(info.Size())
		if d.CountDiskBlocks {
			if bytes, _, linkCount, ok := fileDiskBytes(info); ok {
				freed = bytes
				if linkCount > 1 {
					freed = 0 // another link keeps the file
				}
			}
		}
		d.mu.Lock()
		d.deletedFileCount++
		if !cloudOnly {
			d.deletedBytes += freed
		}
		d.mu.Unlock()
	}
}

func (d *Deletion) failed(path string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failedEntryCount++
	if len(d.errors) < maximumReportedErrors {
		if _, namesPath := err.(*fs.PathError); namesPath {
			d.errors = append(d.errors, err.Error())
		} else {
			d.errors = append(d.errors, fmt.Sprintf("%s: %v", path, err))
		}
	}
}
