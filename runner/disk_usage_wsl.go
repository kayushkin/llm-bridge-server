package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-runner/internal/diskusage"
	"github.com/kayushkin/llm-bridge/msg"
)

// The WSL half of disk usage: the Linux file system of the distribution the
// runner runs in, which the runner scans and deletes from itself, as its own
// user, never leaving that file system (Windows' drives are mounted under
// /mnt), and the disk files of every WSL distribution, which the Windows
// helper measures. Deleting inside WSL frees space in Linux only; Windows
// gets it back when the distribution's disk file is compacted.

const wslRootPath = "/"

// wslScanParallelism is how many directories a WSL scan reads at once.
const wslScanParallelism = 8

var (
	runnerDistributionOnce sync.Once
	runnerDistributionName string
	runnerDistributionErr  error
)

// runnerDistribution is the name of the WSL distribution the runner runs
// in, read once from how Windows names its root (`\\wsl.localhost\Ubuntu-22.04\`).
func runnerDistribution() (string, error) {
	runnerDistributionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "wslpath", "-w", "/").Output()
		if err != nil {
			runnerDistributionErr = fmt.Errorf("wslpath -w /: %w", err)
			return
		}
		windowsRoot := strings.TrimRight(strings.TrimSpace(string(out)), `\`)
		name := windowsRoot[strings.LastIndex(windowsRoot, `\`)+1:]
		if name == "" || !strings.HasPrefix(windowsRoot, `\\`) {
			runnerDistributionErr = fmt.Errorf("wslpath -w / gave %q, which names no distribution", windowsRoot)
			return
		}
		runnerDistributionName = name
	})
	return runnerDistributionName, runnerDistributionErr
}

// isWSLPath says whether a path the bridge sent is in the WSL file system.
func isWSLPath(path string) bool { return strings.HasPrefix(path, "/") }

// wslDrive describes the WSL file system as a drive.
func wslDrive() (msg.DiskUsageDrive, error) {
	total, free, _, err := filesystemSize(wslRootPath)
	if err != nil {
		return msg.DiskUsageDrive{}, err
	}
	label, err := runnerDistribution()
	if err != nil {
		return msg.DiskUsageDrive{}, err
	}
	return msg.DiskUsageDrive{
		Drive: msg.DiskUsageWSLDrive, Kind: msg.DiskUsageDriveWSL, RootPath: wslRootPath,
		Label: label + " (WSL)", TotalBytes: total, FreeBytes: free,
	}, nil
}

// wslDisks asks the helper for every distribution's disk file and adds what
// Linux uses in the runner's own.
func wslDisks(command string) ([]msg.DiskUsageWSLDisk, error) {
	ctx, cancel := context.WithTimeout(context.Background(), diskUsageAnswerTimeout)
	defer cancel()
	output, err := diskUsageHelper(ctx, command, "wsl-disks").Output()
	if err != nil {
		return nil, fmt.Errorf("list WSL disks: %w", helperError(err))
	}
	var disks []msg.DiskUsageWSLDisk
	if err := json.Unmarshal(output, &disks); err != nil {
		return nil, fmt.Errorf("read the helper's list of WSL disks: %w", err)
	}
	own, err := runnerDistribution()
	if err != nil {
		return nil, err
	}
	total, _, used, err := filesystemSize(wslRootPath)
	if err != nil {
		return nil, err
	}
	for i := range disks {
		if disks[i].Distribution == own {
			disks[i].RunnerRunsInIt = true
			disks[i].FilesystemTotalBytes = total
			disks[i].FilesystemUsedBytes = used
		}
	}
	return disks, nil
}

func (d *DiskUsage) runWSLScan() {
	scanner := &diskusage.Scanner{Keep: diskusage.DefaultKeepPolicy, Parallelism: wslScanParallelism, StayOnDevice: true, CountDiskBlocks: true}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				entries, bytes := scanner.Progress()
				d.mu.Lock()
				d.scans[msg.DiskUsageWSLDrive].state.ScannedEntryCount = entries
				d.scans[msg.DiskUsageWSLDrive].state.ScannedBytes = bytes
				d.mu.Unlock()
			}
		}
	}()
	tree, err := scanner.Scan(wslRootPath, wslRootPath)
	close(done)
	d.finishScan(msg.DiskUsageWSLDrive, tree, err)
}

// runnerOwnPaths are the runner's binary, configuration and logs, which a
// WSL deletion never touches.
func runnerOwnPaths() ([]string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return []string{executable, filepath.Dir(configPath()), filepath.Dir(defaultServiceLogPath("runner"))}, nil
}

// wslDeletionVerdict applies the WSL rules and the machine's checks to path.
func wslDeletionVerdict(path string, kind msg.DiskUsageEntryKind) diskusage.Verdict {
	ownPaths, err := runnerOwnPaths()
	if err != nil {
		return diskusage.Verdict{RefusalReason: fmt.Sprintf("the runner could not tell where its own files are (%v), so it deletes nothing", err)}
	}
	layout, err := diskusage.ReadLinuxLayout(ownPaths)
	if err != nil {
		return diskusage.Verdict{RefusalReason: fmt.Sprintf("the runner could not tell where its home folder is (%v), so it deletes nothing", err)}
	}
	rootDevice, err := diskusage.DeviceOfPath(wslRootPath)
	if err != nil {
		return diskusage.Verdict{RefusalReason: fmt.Sprintf("could not read WSL's own file system: %v", err)}
	}
	verdict := diskusage.CheckLinuxDeletion(layout, rootDevice, path, kind)
	if verdict.Allowed {
		verdict.Warnings = append(verdict.Warnings, "This frees space inside WSL. Windows gets the space back only when the WSL disk is compacted.")
	}
	return verdict
}

// runWSLDeletion deletes path inside WSL, reporting progress into the
// deletion the caller claimed, and finishes it.
func (d *DiskUsage) runWSLDeletion(path string, kind msg.DiskUsageEntryKind) {
	deletion := &diskusage.Deletion{StayOnDevice: true, CountDiskBlocks: true}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				progress := deletion.Progress()
				d.mu.Lock()
				d.deletion.DeletedFileCount, d.deletion.DeletedBytes, d.deletion.FailedEntryCount = progress.DeletedFileCount, progress.DeletedBytes, progress.FailedEntryCount
				d.mu.Unlock()
			}
		}
	}()
	err := deletion.Run(path, kind)
	close(done)
	result := helperDeleteResult{DeletionProgress: deletion.Progress()}
	if err != nil {
		result.Error = err.Error()
	}
	log.Printf("[disk-usage] WSL deletion of %s ended", path)
	d.endDeletion(&result, nil, "")
}
