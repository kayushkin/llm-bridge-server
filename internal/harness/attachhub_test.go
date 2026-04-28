package harness

import (
	"bytes"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// testHarness pairs a synthesized PTYProcess with the slave end of a
// real pty so tests can drive bytes from "the upstream CLI" without
// spawning a subprocess.
type testHarness struct {
	pp    *PTYProcess
	slave *os.File
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	pp := &PTYProcess{
		sessionID: "bid-test",
		tty:       master,
		done:      make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-pp.done:
		default:
			close(pp.done)
		}
		_ = slave.Close()
		_ = master.Close()
	})
	return &testHarness{pp: pp, slave: slave}
}

// drainOnce reads one frame off a client's output channel with a
// generous timeout, failing the test if nothing arrives.
func drainOnce(t *testing.T, c *AttachClient) []byte {
	t.Helper()
	select {
	case data := <-c.Out():
		return data
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for hub output frame")
		return nil
	}
}

// TestAttachHub_MultiReaderSingleWriter asserts that the first attacher
// is the writer and a second attacher is a reader whose binary writes
// are silently dropped (the pty never receives them). Both clients
// receive output frames the hub broadcasts from the slave end.
func TestAttachHub_MultiReaderSingleWriter(t *testing.T) {
	th := newTestHarness(t)
	hub := NewAttachHub(th.pp, 4096)

	writer, _, _, err := hub.Attach()
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	reader, _, _, err := hub.Attach()
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}

	if !hub.IsWriter(writer) {
		t.Fatalf("first attacher should be writer")
	}
	if hub.IsWriter(reader) {
		t.Fatalf("second attacher must NOT be writer")
	}

	// Both clients should see output that the upstream "CLI" prints.
	if _, err := th.slave.Write([]byte("hello")); err != nil {
		t.Fatalf("slave write: %v", err)
	}
	if got := string(drainOnce(t, writer)); got != "hello" {
		t.Errorf("writer got %q, want %q", got, "hello")
	}
	if got := string(drainOnce(t, reader)); got != "hello" {
		t.Errorf("reader got %q, want %q", got, "hello")
	}

	// Reader-side Write must be a silent drop — the pty slave should
	// see nothing on its read side. Trailing newline keeps us out of
	// the canonical-mode line buffer if anything sneaks through.
	if err := hub.Write(reader, []byte("ignored\n")); err != nil {
		t.Errorf("reader Write returned error %v, want nil (silent drop)", err)
	}
	th.slave.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 64)
	n, _ := th.slave.Read(buf)
	if n != 0 {
		t.Errorf("reader's bytes leaked into pty: %q", buf[:n])
	}

	// Writer-side Write should reach the slave. Newline so canonical
	// mode flushes the line immediately.
	th.slave.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := hub.Write(writer, []byte("typed\n")); err != nil {
		t.Fatalf("writer Write: %v", err)
	}
	n, err = th.slave.Read(buf)
	if err != nil {
		t.Fatalf("slave read: %v", err)
	}
	// Pty echoes input back; we just want to confirm "typed" is in the
	// data the kernel forwarded to the slave, not exact-match.
	if !bytes.Contains(buf[:n], []byte("typed")) {
		t.Errorf("slave got %q, want substring %q", buf[:n], "typed")
	}
}

// TestAttachHub_WriterPromotion verifies that detaching the writer
// promotes the next remaining reader to writer so the session stays
// usable. Important for the "writer disconnects, reader stays attached"
// flow described in PTY-MODE.md.
func TestAttachHub_WriterPromotion(t *testing.T) {
	th := newTestHarness(t)
	hub := NewAttachHub(th.pp, 4096)

	writer, _, _, _ := hub.Attach()
	reader, _, _, _ := hub.Attach()
	if !hub.IsWriter(writer) || hub.IsWriter(reader) {
		t.Fatalf("initial roles wrong")
	}

	hub.Detach(writer)
	if !hub.IsWriter(reader) {
		t.Fatalf("reader should have been promoted to writer after detach")
	}

	// Promoted client's writes now reach the pty. Newline keeps us out
	// of the canonical-mode line buffer.
	th.slave.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := hub.Write(reader, []byte("promoted\n")); err != nil {
		t.Fatalf("promoted Write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := th.slave.Read(buf)
	if err != nil {
		t.Fatalf("slave read: %v", err)
	}
	if !bytes.Contains(buf[:n], []byte("promoted")) {
		t.Errorf("slave got %q, want substring %q", buf[:n], "promoted")
	}
}

// TestAttachHub_LateAttachReplay asserts that bytes produced before a
// client attaches are replayed in the snapshot returned from Attach,
// up to the ring buffer's configured capacity.
func TestAttachHub_LateAttachReplay(t *testing.T) {
	th := newTestHarness(t)
	hub := NewAttachHub(th.pp, 16) // tiny ring to exercise wrap

	// Produce more bytes than the ring can hold so the replay is the
	// most-recent suffix only.
	if _, err := th.slave.Write([]byte("0123456789ABCDEFGHIJ")); err != nil {
		t.Fatalf("slave write: %v", err)
	}
	// Give the read pump a moment to drain.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		filled := hub.ringFull
		hub.mu.Unlock()
		if filled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, replay, _, err := hub.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := len(replay); got != 16 {
		t.Errorf("replay len = %d, want 16", got)
	}
	// Last 16 bytes of "0123456789ABCDEFGHIJ" are "456789ABCDEFGHIJ".
	want := []byte("456789ABCDEFGHIJ")
	if !bytes.Equal(replay, want) {
		t.Errorf("replay = %q, want %q", replay, want)
	}
}

// TestAttachHub_ResizeWriterOnly asserts that resize control messages
// from a non-writer are silently dropped (no error, no fd change), and
// that a writer's resize calls pty.Setsize and stashes the new size.
func TestAttachHub_ResizeWriterOnly(t *testing.T) {
	th := newTestHarness(t)
	hub := NewAttachHub(th.pp, 4096)

	writer, _, _, _ := hub.Attach()
	reader, _, _, _ := hub.Attach()

	// Reader-side resize: silent drop, last size unchanged.
	if err := hub.Resize(reader, 50, 200); err != nil {
		t.Errorf("reader Resize returned %v, want nil", err)
	}
	hub.mu.Lock()
	if hub.lastSize.Rows == 50 || hub.lastSize.Cols == 200 {
		t.Errorf("reader's resize leaked into lastSize: %+v", hub.lastSize)
	}
	hub.mu.Unlock()

	// Writer-side resize: applied to the fd and stashed.
	if err := hub.Resize(writer, 40, 120); err != nil {
		t.Fatalf("writer Resize: %v", err)
	}
	hub.mu.Lock()
	got := hub.lastSize
	hub.mu.Unlock()
	if got.Rows != 40 || got.Cols != 120 {
		t.Errorf("lastSize = %+v, want 40x120", got)
	}
	// Confirm pty.Getsize sees the same numbers — proves Setsize fired.
	rows, cols, err := pty.Getsize(th.pp.tty)
	if err != nil {
		t.Fatalf("Getsize: %v", err)
	}
	if rows != 40 || cols != 120 {
		t.Errorf("pty.Getsize = %dx%d, want 40x120", rows, cols)
	}
}

// TestAttachHub_BroadcastConcurrency hammers the hub with concurrent
// attach/detach + slave writes to surface races under -race. No
// assertions beyond "doesn't panic or block forever."
func TestAttachHub_BroadcastConcurrency(t *testing.T) {
	th := newTestHarness(t)
	hub := NewAttachHub(th.pp, 1024)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Producer: keep writing slave output.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			th.slave.Write([]byte("x"))
			if i%32 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Consumers: attach, drain a few frames, detach.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				c, _, _, err := hub.Attach()
				if err != nil {
					return
				}
				timeout := time.After(500 * time.Millisecond)
			drain:
				for k := 0; k < 4; k++ {
					select {
					case <-c.Out():
					case <-timeout:
						break drain
					}
				}
				hub.Detach(c)
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
