package harness

import (
	"context"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestRequestDiskUsagePairsTheAnswerWithItsRequest(t *testing.T) {
	registry := NewRunnerRegistry()
	conn := &RunnerConnection{
		MachineName: "remote", MachineID: "m_remote", registry: registry,
		outgoing: make(chan *msg.RunnerMessage, 4), sessions: map[string]*RunnerProcess{},
		closedSignal: make(chan struct{}),
	}
	registry.register(conn)

	go func() {
		sent := <-conn.outgoing
		// An answer to some other request is not this one's.
		conn.handleDiskUsage(&msg.RunnerMessage{Type: msg.RunnerMsgDiskUsage, DiskUsage: &msg.RunnerDiskUsage{RequestID: "someone-else", Answer: &msg.DiskUsageAnswer{Error: "wrong"}}})
		conn.handleDiskUsage(&msg.RunnerMessage{Type: msg.RunnerMsgDiskUsage, DiskUsage: &msg.RunnerDiskUsage{
			RequestID: sent.DiskUsage.RequestID,
			Answer:    &msg.DiskUsageAnswer{Children: &msg.DiskUsageChildren{Drive: sent.DiskUsage.Request.Path[:2]}},
		}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := conn.RequestDiskUsage(ctx, msg.DiskUsageRequest{Action: msg.DiskUsageActionListChildren, Path: `D:\Games`})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Error != "" || answer.Children == nil || answer.Children.Drive != "D:" {
		t.Fatalf("answer %+v", answer)
	}
	if len(conn.diskUsageWaiters) != 0 {
		t.Fatalf("a waiter was left behind: %v", conn.diskUsageWaiters)
	}
}

func TestRequestDiskUsageEndsWhenTheConnectionCloses(t *testing.T) {
	registry := NewRunnerRegistry()
	var disconnected []string
	registry.SetDisconnectReceiver(func(machineID string) { disconnected = append(disconnected, machineID) })
	var finished []msg.DiskUsageDeletion
	registry.SetDiskUsageDeletionReceiver(func(machineID string, deletion msg.DiskUsageDeletion) {
		if machineID != "m_remote" {
			t.Errorf("deletion filed under %s", machineID)
		}
		finished = append(finished, deletion)
	})
	conn := &RunnerConnection{
		MachineName: "remote", MachineID: "m_remote", registry: registry, conn: nil,
		outgoing: make(chan *msg.RunnerMessage, 4), sessions: map[string]*RunnerProcess{},
		closedSignal: make(chan struct{}),
	}
	conn.handleDiskUsage(&msg.RunnerMessage{Type: msg.RunnerMsgDiskUsage, DiskUsage: &msg.RunnerDiskUsage{
		DeletionFinished: &msg.DiskUsageDeletionFinished{Deletion: msg.DiskUsageDeletion{DeletionID: "machine_disk_deletion_000001", State: msg.DiskUsageDeletionCompleted}},
	}})
	if len(finished) != 1 || finished[0].DeletionID != "machine_disk_deletion_000001" {
		t.Fatalf("finished %+v", finished)
	}

	done := make(chan error, 1)
	go func() {
		_, err := conn.RequestDiskUsage(context.Background(), msg.DiskUsageRequest{Action: msg.DiskUsageActionStatus})
		done <- err
	}()
	<-conn.outgoing
	conn.mu.Lock()
	conn.closed = true
	close(conn.closedSignal)
	conn.mu.Unlock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request on a closed connection succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request on a closed connection kept waiting")
	}
}
