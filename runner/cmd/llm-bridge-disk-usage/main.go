// llm-bridge-disk-usage measures what fills a Windows machine's drives and
// deletes what the machine's owner chooses, for the bridge's Machines page.
// The runner in WSL starts it through WSL's Windows interop, so it reads
// and deletes as the Windows user, at Windows speed, rather than through
// WSL's slow view of /mnt/c.
//
//	llm-bridge-disk-usage -version
//	llm-bridge-disk-usage drives                 JSON list of fixed drives
//	llm-bridge-disk-usage wsl-disks              JSON list of WSL distributions' disk files and their sizes
//	llm-bridge-disk-usage scan C:                JSON tree on stdout; "progress <entries> <bytes>" lines on stderr
//	llm-bridge-disk-usage check <path> <kind>    JSON verdict: may it be deleted?
//	llm-bridge-disk-usage delete <path> <kind>   JSON verdict line, then, if allowed, deletes and prints a JSON result line;
//	                                             "progress <files> <bytes> <failed>" lines on stderr
//
// <kind> is "file" or "directory", what the owner saw. Deleting is for
// good: nothing goes to the Recycle Bin. Package internal/diskusage holds
// the rules for what may be deleted.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge-server/runner/internal/diskusage"
	"github.com/kayushkin/llm-bridge/msg"
)

// Version is set at build time (-ldflags "-X main.Version=…").
var Version = "devel"

// progressInterval is how often scan and delete report progress on stderr.
const progressInterval = time.Second

// scanParallelism is how many directories a scan reads at once.
const scanParallelism = 8

func main() {
	if len(os.Args) < 2 {
		fail("usage: llm-bridge-disk-usage -version | drives | wsl-disks | scan <drive> | check <path> <kind> | delete <path> <kind>")
	}
	switch command := os.Args[1]; {
	case command == "-version" && len(os.Args) == 2:
		fmt.Println(Version)
	case command == "drives" && len(os.Args) == 2:
		drives, err := diskusage.Drives()
		if err != nil {
			fail("list drives: %v", err)
		}
		printJSON(drives)
	case command == "wsl-disks" && len(os.Args) == 2:
		disks, err := diskusage.WSLDisks()
		if err != nil {
			fail("list WSL disks: %v", err)
		}
		printJSON(disks)
	case command == "scan" && len(os.Args) == 3:
		scan(os.Args[2])
	case command == "check" && len(os.Args) == 4:
		printJSON(diskusage.CheckDeletion(os.Args[2], msg.DiskUsageEntryKind(os.Args[3])))
	case command == "delete" && len(os.Args) == 4:
		deletePath(os.Args[2], msg.DiskUsageEntryKind(os.Args[3]))
	default:
		fail("unknown command or wrong number of arguments: %q", os.Args[1:])
	}
}

func scan(drive string) {
	if err := diskusage.RequireFixedDrive(drive); err != nil {
		fail("%v", err)
	}
	scanner := &diskusage.Scanner{Keep: diskusage.DefaultKeepPolicy, Parallelism: scanParallelism}
	stop := reportEvery(func() {
		entries, bytes := scanner.Progress()
		fmt.Fprintf(os.Stderr, "progress %d %d\n", entries, bytes)
	})
	tree, err := scanner.Scan(drive+`\`, drive+`\`)
	stop()
	if err != nil {
		fail("scan %s: %v", drive, err)
	}
	printJSON(tree)
}

// deleteResult is the last line delete prints.
type deleteResult struct {
	diskusage.DeletionProgress
	Error string `json:"error,omitempty"` // it stopped before deleting what it could
}

func deletePath(path string, kind msg.DiskUsageEntryKind) {
	verdict := diskusage.CheckDeletion(path, kind)
	printJSON(verdict)
	if !verdict.Allowed {
		return
	}
	var deletion diskusage.Deletion
	stop := reportEvery(func() {
		progress := deletion.Progress()
		fmt.Fprintf(os.Stderr, "progress %d %d %d\n", progress.DeletedFileCount, progress.DeletedBytes, progress.FailedEntryCount)
	})
	err := deletion.Run(path, kind)
	stop()
	result := deleteResult{DeletionProgress: deletion.Progress()}
	if err != nil {
		result.Error = err.Error()
	}
	printJSON(result)
}

// reportEvery calls report every progressInterval until the returned stop
// is called, and once more then.
func reportEvery(report func()) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				report()
				return
			case <-ticker.C:
				report()
			}
		}
	}()
	return func() { close(done); <-finished }
}

func printJSON(value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		fail("encode: %v", err)
	}
	os.Stdout.Write(append(encoded, '\n'))
}

func fail(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "llm-bridge-disk-usage: "+format+"\n", arguments...)
	os.Exit(1)
}
