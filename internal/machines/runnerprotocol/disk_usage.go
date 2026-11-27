package runnerprotocol

import "github.com/kayushkin/llm-bridge/msg"

// Disk usage over the runner connection. The frame lives here; the values it
// carries (requests, answers, scans, entries, deletions) are llm-bridge's
// msg/disk_usage.go, because the server's Machines routes return them too.
//
// The server asks with a RunnerDiskUsage carrying a Request and a RequestID
// it mints; the runner answers with the same RequestID and an Answer. A
// deletion runs on after its answer, and the runner reports how it ended
// with a DeletionFinished message carrying the server's DeletionID.

// RunnerMsgDiskUsage carries a disk-usage request (server → runner), its
// answer, or a finished deletion (runner → server).
const RunnerMsgDiskUsage RunnerMessageType = "disk_usage"

// RunnerDiskUsage is the payload of a RunnerMsgDiskUsage frame. Exactly one
// of Request, Answer and DeletionFinished is set.
type RunnerDiskUsage struct {
	// RequestID pairs a Request with its Answer. The server mints it.
	RequestID        string                         `json:"request_id,omitempty"`
	Request          *msg.DiskUsageRequest          `json:"request,omitempty"`
	Answer           *msg.DiskUsageAnswer           `json:"answer,omitempty"`
	DeletionFinished *msg.DiskUsageDeletionFinished `json:"deletion_finished,omitempty"`
}
