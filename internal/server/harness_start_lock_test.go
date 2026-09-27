package server

import (
	"context"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// TestStartOnInstanceSpawnsOneProcessWhenCalledConcurrently is the race a
// restart opens: auto-resume starts an interrupted session in a goroutine
// while the server is already answering, and a message sent to the same
// session in that gap used to find no process and start a second one. Every
// concurrent caller must get the one process, and the manager must hold it.
func TestStartOnInstanceSpawnsOneProcessWhenCalledConcurrently(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	const bridgeID = "br_concurrent_start"
	if err := st.CreateSession(&store.Session{SessionID: bridgeID, Harness: msg.HarnessMock, InstanceID: instID, State: string(msg.SessionIdle)}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	inst, err := srv.harnessStore.GetInstance(instID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	t.Cleanup(func() { srv.harness.Kill(bridgeID) })

	const callers = 8
	processes := make([]harness.HarnessProcess, callers)
	errs := make([]error, callers)
	var ready, done sync.WaitGroup
	ready.Add(1)
	for i := range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Wait()
			sess, getErr := st.GetSession(bridgeID)
			if getErr != nil {
				errs[i] = getErr
				return
			}
			processes[i], errs[i] = srv.startOnInstance(context.Background(), sess, inst, "")
		}()
	}
	ready.Done()
	done.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: startOnInstance: %v", i, errs[i])
		}
		if processes[i] != processes[0] {
			t.Fatalf("caller %d got a different process from caller 0: %d concurrent starts spawned more than one harness", i, callers)
		}
	}
	if registered := srv.harness.Get(bridgeID); registered != processes[0] {
		t.Fatalf("the manager holds a different process from the one every caller was given")
	}
}
