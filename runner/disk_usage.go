package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/runnerprotocol"
	"github.com/kayushkin/llm-bridge-server/runner/internal/diskusage"
	"github.com/kayushkin/llm-bridge/msg"
)

// Disk usage: the Machines page asks a runner what fills its machine's
// drives and asks it to delete what the owner picks (runnerprotocol/disk_usage.go
// for the frame, llm-bridge msg/disk_usage.go for the values it carries).
//
// In WSL the runner does the work with the Windows build of
// llm-bridge-disk-usage (cmd/llm-bridge-disk-usage), kept under the Windows
// user's local app data and current with the bridge's build, as the
// computer-use tool is. It runs at Windows speed, where reading /mnt/c from
// WSL would take hours on a full drive. Each drive's latest scan stays in
// this process's memory, across connections, until a newer one replaces it.
//
// A deletion must name an entry the latest scan holds, and the runner adds
// its own refusals (a deletion already running, files that live only in
// the cloud) before the helper applies the machine's rules. Other systems
// have no helper yet.

const diskUsageToolName = "disk-usage" // its /api/runner/binary name

// diskUsageAnswerTimeout bounds a helper command the bridge waits on.
const diskUsageAnswerTimeout = 30 * time.Second

var driveLetterForm = regexp.MustCompile(`^[A-Z]:$`)

// installCurrentDiskUsageTool installs or updates this machine's disk-usage
// helper and describes it, or returns nil with no error on a machine that
// has none.
func installCurrentDiskUsageTool(serverURL string) (*msg.DiskUsageTool, error) {
	if !runningInWSLWithInterop() {
		return nil, nil
	}
	localAppData, err := windowsLocalAppDataInWSL()
	if err != nil {
		return nil, err
	}
	command := filepath.Join(localAppData, "llm-bridge", "llm-bridge-disk-usage.exe")
	endpoint, err := buildBinaryURLFor(serverURL, diskUsageToolName, "windows", "amd64")
	if err != nil {
		return nil, err
	}
	if _, err := installCurrentBinary(endpoint, command); err != nil {
		return nil, fmt.Errorf("install the disk-usage helper at %s: %w", command, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	version, err := diskUsageHelper(ctx, command, "-version").Output()
	if err != nil {
		return nil, fmt.Errorf("%s -version: %w", command, err)
	}
	return &msg.DiskUsageTool{Command: command, Version: strings.TrimSpace(string(version))}, nil
}

// diskUsageHelper is the helper run with arguments, with none of the
// server's secrets in its environment. cmd.exe and Windows programs complain
// when started in a Linux directory, so it starts in /mnt/c.
func diskUsageHelper(ctx context.Context, command string, arguments ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command, arguments...)
	cmd.Dir = "/mnt/c"
	cmd.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	return cmd
}

// DiskUsage holds this machine's scans and its deletion, and answers the
// bridge's disk-usage requests.
type DiskUsage struct {
	outgoing chan<- *runnerprotocol.RunnerMessage

	mu       sync.Mutex
	command  string // the helper; "" when this runner has none
	scans    map[string]*driveScan
	deletion *msg.DiskUsageDeletion // running now, or the last one
}

type driveScan struct {
	state msg.DiskUsageScan
	// tree is the newest finished scan, which the page browses, kept while
	// a newer one runs; nil until one finishes.
	tree           *diskusage.Node
	treeFinishedAt time.Time
}

func NewDiskUsage(outgoing chan<- *runnerprotocol.RunnerMessage) *DiskUsage {
	return &DiskUsage{outgoing: outgoing, scans: map[string]*driveScan{}}
}

// SetHelper records the helper installed for this connection, or "".
func (d *DiskUsage) SetHelper(command string) {
	d.mu.Lock()
	d.command = command
	d.mu.Unlock()
}

// Handle answers one request from the bridge.
func (d *DiskUsage) Handle(requestID string, request *msg.DiskUsageRequest) {
	answer, err := d.answer(request)
	if err != nil {
		answer = &msg.DiskUsageAnswer{Error: err.Error()}
	}
	d.outgoing <- &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgDiskUsage, DiskUsage: &runnerprotocol.RunnerDiskUsage{RequestID: requestID, Answer: answer}}
}

func (d *DiskUsage) answer(request *msg.DiskUsageRequest) (*msg.DiskUsageAnswer, error) {
	d.mu.Lock()
	command := d.command
	d.mu.Unlock()
	if command == "" {
		return nil, fmt.Errorf("this runner has no disk-usage helper (only a runner in WSL has one; its log says why it could not install it)")
	}
	switch request.Action {
	case msg.DiskUsageActionStatus:
		status, err := d.status(command)
		return &msg.DiskUsageAnswer{Status: status}, err
	case msg.DiskUsageActionStartScan:
		if err := d.startScan(command, request.Drive); err != nil {
			return nil, err
		}
		status, err := d.status(command)
		return &msg.DiskUsageAnswer{Status: status}, err
	case msg.DiskUsageActionListChildren:
		children, err := d.listChildren(request.Path)
		return &msg.DiskUsageAnswer{Children: children}, err
	case msg.DiskUsageActionCheckDeletion:
		check, err := d.checkDeletion(command, request.Path)
		return &msg.DiskUsageAnswer{DeletionCheck: check}, err
	case msg.DiskUsageActionDelete:
		return d.startDeletion(command, request)
	}
	return nil, fmt.Errorf("unknown disk-usage action %q", request.Action)
}

func (d *DiskUsage) status(command string) (*msg.DiskUsageStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), diskUsageAnswerTimeout)
	defer cancel()
	output, err := diskUsageHelper(ctx, command, "drives").Output()
	if err != nil {
		return nil, fmt.Errorf("list drives: %w", helperError(err))
	}
	status := &msg.DiskUsageStatus{}
	if err := json.Unmarshal(output, &status.Drives); err != nil {
		return nil, fmt.Errorf("read the helper's list of drives: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	status.Scans = []msg.DiskUsageScan{}
	for _, drive := range status.Drives {
		if scan := d.scans[drive.Drive]; scan != nil {
			status.Scans = append(status.Scans, scan.describe())
		}
	}
	if d.deletion != nil {
		deletion := *d.deletion
		status.Deletion = &deletion
	}
	return status, nil
}

func (scan *driveScan) describe() msg.DiskUsageScan {
	described := scan.state
	if scan.tree != nil {
		if scan.state.State == msg.DiskUsageScanFinished {
			root := scan.tree.Entry(scan.state.Drive + `\`)
			described.Root = &root
		} else {
			finishedAt := scan.treeFinishedAt
			described.PreviousFinishedAt = &finishedAt
		}
	}
	return described
}

func (d *DiskUsage) startScan(command, drive string) error {
	if !driveLetterForm.MatchString(drive) {
		return fmt.Errorf("%q is not a drive like C:", drive)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	scan := d.scans[drive]
	if scan == nil {
		scan = &driveScan{}
		d.scans[drive] = scan
	}
	if scan.state.State == msg.DiskUsageScanRunning {
		return fmt.Errorf("%s is already being scanned", drive)
	}
	scan.state = msg.DiskUsageScan{Drive: drive, State: msg.DiskUsageScanRunning, StartedAt: time.Now().UTC()}
	go d.runScan(command, drive)
	return nil
}

func (d *DiskUsage) runScan(command, drive string) {
	tree, err := d.scanWithHelper(command, drive)
	finishedAt := time.Now().UTC()
	d.mu.Lock()
	defer d.mu.Unlock()
	scan := d.scans[drive]
	scan.state.FinishedAt = &finishedAt
	if err != nil {
		log.Printf("[disk-usage] scan of %s failed: %v", drive, err)
		scan.state.State = msg.DiskUsageScanFailed
		scan.state.Error = err.Error()
		return
	}
	log.Printf("[disk-usage] scanned %s: %d bytes in %d files, %s", drive, tree.SizeBytes, tree.FileCount, finishedAt.Sub(scan.state.StartedAt).Round(time.Second))
	scan.state.State = msg.DiskUsageScanFinished
	scan.tree = tree
	scan.treeFinishedAt = finishedAt
}

func (d *DiskUsage) scanWithHelper(command, drive string) (*diskusage.Node, error) {
	cmd := diskUsageHelper(context.Background(), command, "scan", drive)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var stderrTail tail
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			fields := strings.Fields(lines.Text())
			if len(fields) == 3 && fields[0] == "progress" {
				entries, _ := strconv.ParseInt(fields[1], 10, 64)
				bytes, _ := strconv.ParseUint(fields[2], 10, 64)
				d.mu.Lock()
				d.scans[drive].state.ScannedEntryCount = entries
				d.scans[drive].state.ScannedBytes = bytes
				d.mu.Unlock()
				continue
			}
			stderrTail.add(lines.Text())
		}
	}()
	var tree diskusage.Node
	decodeErr := json.NewDecoder(stdout).Decode(&tree)
	io.Copy(io.Discard, stdout)
	<-stderrDone
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderrTail.String())
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("read the scan: %w", decodeErr)
	}
	return &tree, nil
}

// find returns the scan of path's drive and the node at path in it.
// The caller holds d.mu.
func (d *DiskUsage) find(path string) (*driveScan, *diskusage.Node, []*diskusage.Node, error) {
	if len(path) < 3 || !driveLetterForm.MatchString(path[:2]) || path[2] != '\\' {
		return nil, nil, nil, fmt.Errorf("%q is not a path on a drive", path)
	}
	scan := d.scans[path[:2]]
	if scan == nil || scan.tree == nil {
		return nil, nil, nil, fmt.Errorf("%s has not been scanned since the runner started; scan it first", path[:2])
	}
	node, ancestors := scan.tree.Find(path[:3], path)
	if node == nil {
		return nil, nil, nil, fmt.Errorf("the latest scan of %s holds no %s", path[:2], path)
	}
	return scan, node, ancestors, nil
}

func (d *DiskUsage) listChildren(path string) (*msg.DiskUsageChildren, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	scan, node, _, err := d.find(path)
	if err != nil {
		return nil, err
	}
	children := &msg.DiskUsageChildren{Drive: path[:2], ScannedAt: scan.treeFinishedAt, Entry: node.Entry(path), Children: []msg.DiskUsageEntry{}}
	for _, child := range node.Children {
		children.Children = append(children.Children, child.Entry(diskusage.ChildPath(path, child.Name)))
	}
	return children, nil
}

// runnerDeletionCheck is what the runner alone says about deleting path:
// what the scan knows of it, and the refusals and warnings the helper
// cannot give. The caller holds d.mu.
func (d *DiskUsage) runnerDeletionCheck(path string) (*msg.DiskUsageDeletionCheck, error) {
	scan, node, _, err := d.find(path)
	if err != nil {
		return nil, err
	}
	check := &msg.DiskUsageDeletionCheck{
		Path:             path,
		Kind:             node.Kind,
		SizeBytes:        node.SizeBytes,
		FileCount:        node.FileCount,
		DriveUsedBytes:   scan.tree.SizeBytes,
		ConfirmationName: node.Name,
		ScannedAt:        scan.treeFinishedAt,
		Allowed:          true,
	}
	switch {
	case node.Kind != msg.DiskUsageEntryFile && node.Kind != msg.DiskUsageEntryDirectory:
		check.Allowed, check.RefusalReason = false, fmt.Sprintf("only a file or a folder can be deleted here, and this is a %s", node.Kind)
	case node.CloudOnlyFileCount > 0:
		check.Allowed, check.RefusalReason = false, fmt.Sprintf("%d files under it live only in the cloud (OneDrive or similar); deleting them would delete them from the cloud, so delete them there", node.CloudOnlyFileCount)
	case d.deletion != nil && d.deletion.State == msg.DiskUsageDeletionRunning:
		check.Allowed, check.RefusalReason = false, fmt.Sprintf("a deletion of %s is still running; wait for it to finish", d.deletion.Path)
	}
	if node.UnreadableDirectoryCount > 0 {
		check.Warnings = append(check.Warnings, fmt.Sprintf("The scan could not read %d folders under this, so it may hold more than it shows.", node.UnreadableDirectoryCount))
	}
	if node.PartlyDeleted {
		check.Warnings = append(check.Warnings, "An earlier deletion removed part of this; its size here is a guess until you scan again.")
	}
	if scan.tree.SizeBytes > 0 && node.SizeBytes*2 > scan.tree.SizeBytes {
		check.Warnings = append(check.Warnings, fmt.Sprintf("This is %d%% of everything the scan found on %s.", node.SizeBytes*100/scan.tree.SizeBytes, path[:2]))
	}
	return check, nil
}

func (d *DiskUsage) checkDeletion(command, path string) (*msg.DiskUsageDeletionCheck, error) {
	d.mu.Lock()
	check, err := d.runnerDeletionCheck(path)
	d.mu.Unlock()
	if err != nil || !check.Allowed {
		return check, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), diskUsageAnswerTimeout)
	defer cancel()
	output, err := diskUsageHelper(ctx, command, "check", path, string(check.Kind)).Output()
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", path, helperError(err))
	}
	var verdict diskusage.Verdict
	if err := json.Unmarshal(output, &verdict); err != nil {
		return nil, fmt.Errorf("read the helper's verdict: %w", err)
	}
	mergeVerdict(check, verdict)
	return check, nil
}

func mergeVerdict(check *msg.DiskUsageDeletionCheck, verdict diskusage.Verdict) {
	check.Allowed = verdict.Allowed
	check.RefusalReason = verdict.RefusalReason
	check.Warnings = append(verdict.Warnings, check.Warnings...)
}

func (d *DiskUsage) startDeletion(command string, request *msg.DiskUsageRequest) (*msg.DiskUsageAnswer, error) {
	if request.DeletionID == "" {
		return nil, fmt.Errorf("a deletion needs the bridge's deletion id")
	}
	d.mu.Lock()
	check, err := d.runnerDeletionCheck(request.Path)
	if err == nil && check.Allowed && check.Kind != request.ExpectedKind {
		check.Allowed, check.RefusalReason = false, fmt.Sprintf("the scan holds %s as a %s, not the %s you saw; scan again", request.Path, check.Kind, request.ExpectedKind)
	}
	if err != nil || !check.Allowed {
		d.mu.Unlock()
		return &msg.DiskUsageAnswer{DeletionCheck: check}, err
	}
	// Claim the one deletion slot before the helper starts, so a second
	// request cannot slip in while the helper checks this one.
	startedAt := time.Now().UTC()
	previous := d.deletion
	d.deletion = &msg.DiskUsageDeletion{
		DeletionID:        request.DeletionID,
		Path:              request.Path,
		Kind:              check.Kind,
		State:             msg.DiskUsageDeletionRunning,
		StartedAt:         startedAt,
		ExpectedSizeBytes: check.SizeBytes,
	}
	d.mu.Unlock()

	cmd := diskUsageHelper(context.Background(), command, "delete", request.Path, string(check.Kind))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		d.releaseDeletion(previous)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		d.releaseDeletion(previous)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		d.releaseDeletion(previous)
		return nil, fmt.Errorf("start the helper: %w", err)
	}
	lines := bufio.NewReader(stdout)
	first, readErr := lines.ReadBytes('\n')
	var verdict diskusage.Verdict
	if readErr != nil || json.Unmarshal(first, &verdict) != nil {
		cmd.Process.Kill()
		waitErr := cmd.Wait()
		d.releaseDeletion(previous)
		return nil, fmt.Errorf("the helper gave no verdict on %s (%v, %v)", request.Path, readErr, waitErr)
	}
	mergeVerdict(check, verdict)
	if !verdict.Allowed {
		cmd.Wait()
		d.releaseDeletion(previous)
		log.Printf("[disk-usage] refused to delete %s: %s", request.Path, verdict.RefusalReason)
		return &msg.DiskUsageAnswer{DeletionCheck: check}, nil
	}
	log.Printf("[disk-usage] deleting %s (%s, %d bytes in the scan) for deletion %s", request.Path, check.Kind, check.SizeBytes, request.DeletionID)
	go d.finishDeletion(cmd, lines, stderr)

	d.mu.Lock()
	deletion := *d.deletion
	d.mu.Unlock()
	return &msg.DiskUsageAnswer{DeletionCheck: check, Status: &msg.DiskUsageStatus{Deletion: &deletion}}, nil
}

// releaseDeletion gives back the deletion slot when a deletion did not start.
func (d *DiskUsage) releaseDeletion(previous *msg.DiskUsageDeletion) {
	d.mu.Lock()
	d.deletion = previous
	d.mu.Unlock()
}

// helperDeleteResult is the last line `delete` prints.
type helperDeleteResult struct {
	diskusage.DeletionProgress
	Error string `json:"error,omitempty"`
}

func (d *DiskUsage) finishDeletion(cmd *exec.Cmd, stdout *bufio.Reader, stderr io.Reader) {
	var stderrTail tail
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			fields := strings.Fields(lines.Text())
			if len(fields) == 4 && fields[0] == "progress" {
				files, _ := strconv.ParseInt(fields[1], 10, 64)
				bytes, _ := strconv.ParseUint(fields[2], 10, 64)
				failed, _ := strconv.ParseInt(fields[3], 10, 64)
				d.mu.Lock()
				d.deletion.DeletedFileCount, d.deletion.DeletedBytes, d.deletion.FailedEntryCount = files, bytes, failed
				d.mu.Unlock()
				continue
			}
			stderrTail.add(lines.Text())
		}
	}()
	var result helperDeleteResult
	decodeErr := json.NewDecoder(stdout).Decode(&result)
	<-stderrDone
	waitErr := cmd.Wait()

	finishedAt := time.Now().UTC()
	d.mu.Lock()
	deletion := d.deletion
	deletion.FinishedAt = &finishedAt
	switch {
	case waitErr != nil || decodeErr != nil:
		deletion.State = msg.DiskUsageDeletionFailed
		deletion.Errors = append(deletion.Errors, fmt.Sprintf("the helper did not finish cleanly (%v, %v): %s", waitErr, decodeErr, stderrTail.String()))
	default:
		deletion.DeletedBytes = result.DeletedBytes
		deletion.DeletedFileCount = result.DeletedFileCount
		deletion.FailedEntryCount = result.FailedEntryCount
		deletion.Errors = result.Errors
		switch {
		case result.Error != "":
			deletion.State = msg.DiskUsageDeletionFailed
			deletion.Errors = append([]string{result.Error}, deletion.Errors...)
		case result.FailedEntryCount > 0:
			deletion.State = msg.DiskUsageDeletionPartial
		default:
			deletion.State = msg.DiskUsageDeletionCompleted
		}
	}
	d.updateTreeAfterDeletion(deletion)
	finished := *deletion
	d.mu.Unlock()

	log.Printf("[disk-usage] deletion %s of %s %s: %d bytes in %d files deleted, %d entries failed", finished.DeletionID, finished.Path, finished.State, finished.DeletedBytes, finished.DeletedFileCount, finished.FailedEntryCount)
	d.outgoing <- &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgDiskUsage, DiskUsage: &runnerprotocol.RunnerDiskUsage{DeletionFinished: &msg.DiskUsageDeletionFinished{Deletion: finished}}}
}

// updateTreeAfterDeletion takes what a deletion removed out of its drive's
// scan. The caller holds d.mu.
func (d *DiskUsage) updateTreeAfterDeletion(deletion *msg.DiskUsageDeletion) {
	_, node, ancestors, err := d.find(deletion.Path)
	if err != nil {
		return // a newer scan no longer holds it
	}
	if deletion.State == msg.DiskUsageDeletionCompleted {
		diskusage.RemoveDeleted(node, ancestors)
		return
	}
	if deletion.DeletedBytes > 0 || deletion.DeletedFileCount > 0 {
		diskusage.SubtractPartlyDeleted(node, ancestors, deletion.DeletedBytes, deletion.DeletedFileCount)
	}
}

// helperError adds the helper's own message to an exit error.
func helperError(err error) error {
	if exitError, ok := err.(*exec.ExitError); ok && len(exitError.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitError.Stderr)))
	}
	return err
}

// tail keeps the last few lines of a helper's stderr for an error message.
type tail struct {
	lines []string
}

func (t *tail) add(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > 5 {
		t.lines = t.lines[1:]
	}
}

func (t *tail) String() string { return strings.Join(t.lines, "; ") }
