package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/runnerprotocol"
	"github.com/kayushkin/llm-bridge/msg"
)

// Disk usage over the runner connection (runnerprotocol/disk_usage.go): the
// server sends a request with an id it mints and waits for the answer with
// the same id; a runner reports a finished deletion on its own, to the
// registry's receiver, under the machine its token belongs to.

// RequestDiskUsage sends request to the runner and waits for its answer, or
// until ctx ends or the connection closes.
func (c *RunnerConnection) RequestDiskUsage(ctx context.Context, request msg.DiskUsageRequest) (*msg.DiskUsageAnswer, error) {
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	requestID := "disk_usage_request_" + hex.EncodeToString(idBytes)
	answers := make(chan *msg.DiskUsageAnswer, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("the runner on %s disconnected", c.MachineName)
	}
	if c.diskUsageWaiters == nil {
		c.diskUsageWaiters = map[string]chan *msg.DiskUsageAnswer{}
	}
	c.diskUsageWaiters[requestID] = answers
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.diskUsageWaiters, requestID)
		c.mu.Unlock()
	}()

	if err := c.Send(&runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgDiskUsage, DiskUsage: &runnerprotocol.RunnerDiskUsage{RequestID: requestID, Request: &request}}); err != nil {
		return nil, fmt.Errorf("send to the runner on %s: %w", c.MachineName, err)
	}
	select {
	case answer := <-answers:
		return answer, nil
	case <-c.closedSignal:
		return nil, fmt.Errorf("the runner on %s disconnected before it answered", c.MachineName)
	case <-ctx.Done():
		return nil, fmt.Errorf("the runner on %s did not answer: %w", c.MachineName, ctx.Err())
	}
}

// handleDiskUsage routes a runner's answer to its waiter and a finished
// deletion to the registry's receiver.
func (c *RunnerConnection) handleDiskUsage(m *runnerprotocol.RunnerMessage) {
	if m.DiskUsage == nil {
		log.Printf("[runner-server] disk_usage from %s with no payload", c.MachineName)
		return
	}
	if finished := m.DiskUsage.DeletionFinished; finished != nil {
		c.registry.mu.RLock()
		receiver := c.registry.diskUsageDeletionReceiver
		c.registry.mu.RUnlock()
		if receiver == nil {
			log.Printf("[runner-server] %s finished deletion %s, and nothing records it", c.MachineName, finished.Deletion.DeletionID)
			return
		}
		receiver(c.MachineID, finished.Deletion)
		return
	}
	if m.DiskUsage.Answer == nil {
		log.Printf("[runner-server] disk_usage from %s with neither an answer nor a finished deletion", c.MachineName)
		return
	}
	c.mu.Lock()
	waiter := c.diskUsageWaiters[m.DiskUsage.RequestID]
	c.mu.Unlock()
	if waiter == nil {
		log.Printf("[runner-server] %s answered disk-usage request %s, which nobody is waiting for", c.MachineName, m.DiskUsage.RequestID)
		return
	}
	select {
	case waiter <- m.DiskUsage.Answer:
	default:
	}
}

// SetDiskUsageDeletionReceiver sets what records runners' finished deletions.
func (r *RunnerRegistry) SetDiskUsageDeletionReceiver(receiver func(machineID string, deletion msg.DiskUsageDeletion)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.diskUsageDeletionReceiver = receiver
}

// SetDisconnectReceiver sets what is told when a machine's runner
// connection closes.
func (r *RunnerRegistry) SetDisconnectReceiver(receiver func(machineID string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disconnectReceiver = receiver
}
