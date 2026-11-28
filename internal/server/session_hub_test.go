package server

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/sessions"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// upsert is a publishable frame naming one session, so a test can assert on
// which sessions came back and in what order.
func upsert(id string) sessions.SessionListEvent {
	return sessions.SessionListEvent{Type: "upsert", Session: &store.Session{SessionID: id}}
}

func seqsOf(frames []sessions.SessionListFrame) []uint64 {
	out := make([]uint64, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Seq)
	}
	return out
}

func equalSeqs(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSessionHubNumbersFramesFromOne(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	_, ch, backlog, resume := h.Subscribe("")
	if resume != sessions.ResumeNone {
		t.Fatalf("fresh connect resume = %q, want %q", resume, sessions.ResumeNone)
	}
	if len(backlog) != 0 {
		t.Fatalf("fresh connect backlog = %d frames, want 0", len(backlog))
	}

	h.PublishOwnedBy(upsert("a"), "")
	h.PublishOwnedBy(upsert("b"), "")

	first := <-ch
	second := <-ch
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("sequence = %d,%d, want 1,2", first.Seq, second.Seq)
	}
	if got := h.EventID(first.Seq); got != h.StreamID+"-1" {
		t.Fatalf("eventID = %q, want %q", got, h.StreamID+"-1")
	}
}

func TestSessionHubReplaysExactlyWhatWasMissed(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	for i := 0; i < 5; i++ {
		h.PublishOwnedBy(upsert(fmt.Sprintf("s%d", i)), "")
	}

	// A client that saw through frame 2 gets 3, 4 and 5 and nothing else.
	_, _, backlog, resume := h.Subscribe(h.EventID(2))
	if resume != sessions.ResumeReplayed {
		t.Fatalf("resume = %q, want %q", resume, sessions.ResumeReplayed)
	}
	if got := seqsOf(backlog); !equalSeqs(got, []uint64{3, 4, 5}) {
		t.Fatalf("backlog seqs = %v, want [3 4 5]", got)
	}
}

func TestSessionHubUpToDateClientGetsNoBacklog(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	h.PublishOwnedBy(upsert("a"), "")

	_, _, backlog, resume := h.Subscribe(h.EventID(1))
	if resume != sessions.ResumeReplayed {
		t.Fatalf("resume = %q, want %q", resume, sessions.ResumeReplayed)
	}
	if len(backlog) != 0 {
		t.Fatalf("backlog = %v, want empty", seqsOf(backlog))
	}
}

func TestSessionHubReportsGapWhenBufferHasRolledPast(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	for i := 0; i < sessions.SessionListReplayCapacity+10; i++ {
		h.PublishOwnedBy(upsert(fmt.Sprintf("s%d", i)), "")
	}

	// Frame 1 fell out of the buffer long ago. Reporting "replayed" here would
	// hand back a partial list the client would then trust — the exact silent
	// hole this whole path exists to close.
	_, _, backlog, resume := h.Subscribe(h.EventID(1))
	if resume != sessions.ResumeGap {
		t.Fatalf("resume = %q, want %q", resume, sessions.ResumeGap)
	}
	if len(backlog) != 0 {
		t.Fatalf("gap backlog = %v, want empty", seqsOf(backlog))
	}

	// The oldest frame the buffer still fully covers does resume. The buffer
	// holds seqs 11..522, so a client that saw 10 is covered and one that saw 9
	// is not.
	oldestCovered := uint64(10)
	if _, _, b, r := h.Subscribe(h.EventID(oldestCovered)); r != sessions.ResumeReplayed || len(b) != sessions.SessionListReplayCapacity {
		t.Fatalf("resume at boundary = %q with %d frames, want %q with %d",
			r, len(b), sessions.ResumeReplayed, sessions.SessionListReplayCapacity)
	}
	if _, _, _, r := h.Subscribe(h.EventID(oldestCovered - 1)); r != sessions.ResumeGap {
		t.Fatalf("resume one past boundary = %q, want %q", r, sessions.ResumeGap)
	}
}

func TestSessionHubRejectsAnotherProcessesEventID(t *testing.T) {
	old := sessions.NewSessionHub(nil)
	old.PublishOwnedBy(upsert("a"), "")
	old.PublishOwnedBy(upsert("b"), "")

	// A client reconnecting across a server restart carries an id whose numbers
	// mean nothing to the new process. Honoring them would replay frames that
	// happen to share a sequence number but are entirely different sessions.
	fresh := sessions.NewSessionHub(nil)
	for i := 0; i < 5; i++ {
		fresh.PublishOwnedBy(upsert(fmt.Sprintf("t%d", i)), "")
	}
	_, _, backlog, resume := fresh.Subscribe(old.EventID(2))
	if resume != sessions.ResumeGap {
		t.Fatalf("cross-process resume = %q, want %q", resume, sessions.ResumeGap)
	}
	if len(backlog) != 0 {
		t.Fatalf("cross-process backlog = %v, want empty", seqsOf(backlog))
	}
}

func TestSessionHubRejectsUnparseableAndFutureEventIDs(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	h.PublishOwnedBy(upsert("a"), "")

	for _, id := range []string{
		"garbage",
		"-1",
		h.StreamID,
		h.StreamID + "-",
		h.StreamID + "-notanumber",
		h.EventID(99), // ahead of anything this process has published
	} {
		if _, _, _, resume := h.Subscribe(id); resume != sessions.ResumeGap {
			t.Fatalf("resume for Last-Event-ID %q = %q, want %q", id, resume, sessions.ResumeGap)
		}
	}
}

func TestSessionHubBacklogAndLiveChannelDoNotOverlap(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	for i := 0; i < 3; i++ {
		h.PublishOwnedBy(upsert(fmt.Sprintf("s%d", i)), "")
	}

	_, ch, backlog, resume := h.Subscribe(h.EventID(1))
	if resume != sessions.ResumeReplayed {
		t.Fatalf("resume = %q, want %q", resume, sessions.ResumeReplayed)
	}
	h.PublishOwnedBy(upsert("live"), "")

	got := seqsOf(backlog)
	for {
		select {
		case frame := <-ch:
			got = append(got, frame.Seq)
			continue
		default:
		}
		break
	}
	// Every frame after the client's last, once each, in order — no duplicate
	// of a backlog frame arriving again on the channel, no frame lost between
	// the snapshot and the subscription.
	if !equalSeqs(got, []uint64{2, 3, 4}) {
		t.Fatalf("delivered seqs = %v, want [2 3 4]", got)
	}
}

// TestSessionHubJoinsCleanlyUnderConcurrentPublish is the check for the claim
// the subscribe comment makes: the backlog snapshot and the channel
// registration happen under one lock, so a frame published while a client is
// joining lands in exactly one of them. Reversing that order loses a frame or
// delivers it twice, and neither shows up in a quiet test.
//
// The window it has to hit is a few instructions wide, so the check joins many
// subscribers at once across many attempts rather than one: a single joiner
// almost never lands inside it, and a check that cannot reach the window is a
// check of something else.
func TestSessionHubJoinsCleanlyUnderConcurrentPublish(t *testing.T) {
	const (
		attempts  = 300
		joiners   = 16
		published = 64
	)
	for attempt := 0; attempt < attempts; attempt++ {
		h := sessions.NewSessionHub(nil)
		h.PublishOwnedBy(upsert("seed"), "")

		start := make(chan struct{})
		results := make(chan []uint64, joiners)
		failures := make(chan string, joiners)

		var wg sync.WaitGroup
		wg.Add(joiners)
		for j := 0; j < joiners; j++ {
			go func() {
				defer wg.Done()
				<-start
				_, ch, backlog, resume := h.Subscribe(h.EventID(1))
				if resume != sessions.ResumeReplayed {
					failures <- fmt.Sprintf("resume = %q, want %q", resume, sessions.ResumeReplayed)
					return
				}
				// Drain once the publisher is done; the caller waits for both.
				go func() {
					got := seqsOf(backlog)
					// A late join can receive every frame in the replay backlog.
					// No live frame remains to trigger the completion check below.
					if len(got) == published {
						results <- got
						return
					}
					deadline := time.After(2 * time.Second)
					for {
						select {
						case frame := <-ch:
							got = append(got, frame.Seq)
							if len(got) == published {
								results <- got
								return
							}
						case <-deadline:
							results <- got
							return
						}
					}
				}()
			}()
		}

		go func() {
			<-start
			for i := 0; i < published; i++ {
				h.PublishOwnedBy(upsert(fmt.Sprintf("s%d", i)), "")
			}
		}()

		close(start)
		wg.Wait()

		select {
		case msg := <-failures:
			t.Fatalf("attempt %d: %s", attempt, msg)
		default:
		}

		for j := 0; j < joiners; j++ {
			got := <-results
			// Contiguous from 2, no gaps, no repeats — whatever interleaving
			// the scheduler picked.
			for i, seq := range got {
				if seq != uint64(i+2) {
					t.Fatalf("attempt %d joiner %d: delivered seq %d at position %d, want %d (all: %v)",
						attempt, j, seq, i, i+2, got)
				}
			}
			if len(got) != published {
				t.Fatalf("attempt %d joiner %d: delivered %d frames, want %d (%v)",
					attempt, j, len(got), published, got)
			}
		}
	}
}

func TestSessionHubBufferStaysBounded(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	for i := 0; i < sessions.SessionListReplayCapacity*3; i++ {
		h.PublishOwnedBy(upsert(fmt.Sprintf("s%d", i)), "")
	}
	h.Mu.Lock()
	held, capacity := len(h.Replay), cap(h.Replay)
	h.Mu.Unlock()
	if held != sessions.SessionListReplayCapacity {
		t.Fatalf("buffer holds %d frames, want %d", held, sessions.SessionListReplayCapacity)
	}
	if capacity > sessions.SessionListReplayCapacity*2 {
		t.Fatalf("buffer backing array grew to %d, want it bounded near %d",
			capacity, sessions.SessionListReplayCapacity)
	}
}

type sseFrame struct{ id, event, data string }

// frameReadTimeout bounds how long a test waits for a frame the server should
// already have written. A missing frame has to fail with a message naming what
// was missing — left to block, a real defect surfaces as the package timing out
// minutes later with a stack trace, which reads as flakiness rather than as the
// assertion it is.
const frameReadTimeout = 3 * time.Second

// frameStream parses one live SSE response. Exactly one goroutine ever reads
// the connection: a second reader on the same buffered stream can be parked
// mid-send holding a line the other one then never sees.
type frameStream struct {
	resp   *http.Response
	frames chan sseFrame
}

func openFrameStream(t *testing.T, req *http.Request) *frameStream {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	fs := &frameStream{resp: resp, frames: make(chan sseFrame, 64)}
	go func() {
		defer close(fs.frames)
		body := bufio.NewReader(resp.Body)
		var cur sseFrame
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				return
			}
			switch line = strings.TrimRight(line, "\n"); {
			case line == "":
				if cur.event != "" {
					fs.frames <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return fs
}

func (fs *frameStream) close() { fs.resp.Body.Close() }

// take waits for want frames, failing if any of them does not arrive.
func (fs *frameStream) take(t *testing.T, want int) []sseFrame {
	t.Helper()
	out := make([]sseFrame, 0, want)
	for len(out) < want {
		select {
		case frame, ok := <-fs.frames:
			if !ok {
				t.Fatalf("stream ended after %d of %d frames; have %+v", len(out), want, out)
			}
			out = append(out, frame)
		case <-time.After(frameReadTimeout):
			t.Fatalf("timed out waiting for frame %d of %d; have %+v", len(out)+1, want, out)
		}
	}
	return out
}

// TestSessionListEventsReplaysOverHTTP drives the real handler, because the id
// line and the Last-Event-ID header are the wire contract — a hub that numbers
// frames correctly but never writes the id line closes no gap at all.
func TestSessionListEventsReplaysOverHTTP(t *testing.T) {
	hub := sessions.NewSessionHub(nil)
	srv := (&Server{sessionHub: hub}).withServicesComposed()
	ts := httptest.NewServer(http.HandlerFunc(srv.sessions.HandleSessionListEvents))
	defer ts.Close()

	// First connection: no resume, then two live frames.
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	first := openFrameStream(t, req)
	defer first.close()

	frames := first.take(t, 1)
	if frames[0].event != "hello" {
		t.Fatalf("first frame = %q, want hello", frames[0].event)
	}
	if !strings.Contains(frames[0].data, `"resume":"none"`) {
		t.Fatalf("hello data = %s, want resume none", frames[0].data)
	}

	hub.PublishOwnedBy(upsert("a"), "")
	hub.PublishOwnedBy(upsert("b"), "")
	frames = first.take(t, 2)
	if frames[0].id != hub.EventID(1) || frames[1].id != hub.EventID(2) {
		t.Fatalf("frame ids = %q,%q, want %q,%q",
			frames[0].id, frames[1].id, hub.EventID(1), hub.EventID(2))
	}
	lastSeen := frames[0].id // deliberately behind: pretend frame 2 was missed
	if lastSeen == "" {
		t.Fatal("live frame carried no id line; a client has nothing to resume from")
	}
	first.close()

	// Anything published while nobody is connected is exactly what the gap used
	// to swallow.
	hub.PublishOwnedBy(upsert("c"), "")

	req2, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req2.Header.Set("Last-Event-ID", lastSeen)
	second := openFrameStream(t, req2)
	defer second.close()

	frames = second.take(t, 3)
	if frames[0].event != "hello" || !strings.Contains(frames[0].data, `"resume":"replayed"`) {
		t.Fatalf("reconnect hello = %+v, want resume replayed", frames[0])
	}
	if frames[1].id != hub.EventID(2) || !strings.Contains(frames[1].data, `"b"`) {
		t.Fatalf("first replayed frame = %+v, want seq 2 carrying session b", frames[1])
	}
	if frames[2].id != hub.EventID(3) || !strings.Contains(frames[2].data, `"c"`) {
		t.Fatalf("second replayed frame = %+v, want seq 3 carrying session c", frames[2])
	}
}

func TestSessionListEventsTellsClientWhenItCannotResume(t *testing.T) {
	hub := sessions.NewSessionHub(nil)
	srv := (&Server{sessionHub: hub}).withServicesComposed()
	ts := httptest.NewServer(http.HandlerFunc(srv.sessions.HandleSessionListEvents))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Last-Event-ID", "some-other-process-42")
	stream := openFrameStream(t, req)
	defer stream.close()

	frames := stream.take(t, 1)
	if !strings.Contains(frames[0].data, `"resume":"gap"`) {
		t.Fatalf("hello data = %s, want resume gap", frames[0].data)
	}
	if !strings.Contains(frames[0].data, `"stream_id":"`+hub.StreamID+`"`) {
		t.Fatalf("hello data = %s, want this process's stream id", frames[0].data)
	}
}

func TestSessionHubUnsubscribeStopsDelivery(t *testing.T) {
	h := sessions.NewSessionHub(nil)
	id, ch, _, _ := h.Subscribe("")
	h.Unsubscribe(id)
	h.PublishOwnedBy(upsert("a"), "")
	select {
	case frame := <-ch:
		t.Fatalf("unsubscribed channel got frame %d", frame.Seq)
	case <-time.After(50 * time.Millisecond):
	}
}
