package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/peers"

	"github.com/gorilla/websocket"
)

// openPeerSocketPair gives a PeerSocket over a real WebSocket and the frames the
// far end reads from it.
func openPeerSocketPair(t *testing.T) (*peers.PeerSocket, <-chan peers.PeerSocketFrame) {
	t.Helper()
	received := make(chan peers.PeerSocketFrame, 16)
	upgrader := websocket.Upgrader{}
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		for {
			_, data, err := connection.ReadMessage()
			if err != nil {
				return
			}
			var frame peers.PeerSocketFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Errorf("decode frame: %v", err)
				return
			}
			received <- frame
		}
	}))
	t.Cleanup(far.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(far.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	return peers.NewPeerSocket(nil, "peer_connection_000001", connection, true), received
}

func waitForFrame(received <-chan peers.PeerSocketFrame, within time.Duration) (peers.PeerSocketFrame, bool) {
	select {
	case frame := <-received:
		return frame, true
	case <-time.After(within):
		return peers.PeerSocketFrame{}, false
	}
}

// A reader that goes away before the answer ends — a chat closing a session's
// event stream through the proxy — must tell the other server to stop serving
// it. Until 2026-10-29 the stopped read marked the reader done, Close then sent
// nothing, and the other server held the stream's slot until all 64 were held.
func TestPeerSocketBodyCloseAfterCanceledReadSendsCancel(t *testing.T) {
	socket, received := openPeerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &peers.PeerSocketBodyReader{Socket: socket, Stream: 7, Frames: make(chan peers.PeerSocketFrame), Ctx: ctx}
	cancel()
	if _, err := reader.Read(make([]byte, 16)); err == nil {
		t.Fatal("read after cancel returned no error")
	}
	reader.Close()
	frame, ok := waitForFrame(received, 2*time.Second)
	if !ok || frame.Type != peers.PeerSocketFrameCancel || frame.Stream != 7 {
		t.Fatalf("frame after Close = %+v (arrived %v), want cancel for stream 7", frame, ok)
	}
}

// An answer the other server finished needs no cancel.
func TestPeerSocketBodyCloseAfterEndSendsNothing(t *testing.T) {
	socket, received := openPeerSocketPair(t)
	frames := make(chan peers.PeerSocketFrame, 1)
	frames <- peers.PeerSocketFrame{Type: peers.PeerSocketFrameEnd, Stream: 9}
	reader := &peers.PeerSocketBodyReader{Socket: socket, Stream: 9, Frames: frames, Ctx: context.Background()}
	if _, err := reader.Read(make([]byte, 16)); err == nil {
		t.Fatal("read past the end returned no error")
	}
	reader.Close()
	if frame, ok := waitForFrame(received, 300*time.Millisecond); ok {
		t.Fatalf("Close after the end sent %+v, want nothing", frame)
	}
}
