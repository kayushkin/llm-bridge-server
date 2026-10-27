package server

// The dial-out transport between two connected servers (design note
// d107154a-49ac-471b-a1b7-fc7975de37f3, "Decided on 2026-10-27").
//
// When only one of two servers has an address, the other dials out to it at
// GET /peer/socket and keeps the WebSocket open, reconnecting with backoff.
// The socket's credential is the connection's own, which gives no access by
// itself. Requests in both directions are carried over it as frames: a
// request frame (method, path, the few headers that may cross, and the whole
// body), then a response head, body frames as they are written, and an end.
// Each side numbers the streams it opens with its own parity, so the two never
// collide.
//
// A request that arrives over a socket is served by the server's own
// ServeHTTP, so authorizeAndServe judges it exactly as one that arrived by
// https: its peer token decides who it acts as. Only the headers in
// headersCrossingPeerSockets reach the handler, and the request carries the
// socket's connection in its context so authorization can tell it arrived
// this way. Body frames are sent as the handler writes them, so an SSE stream
// stays live.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

const (
	peerSocketFrameRequest      = "request"
	peerSocketFrameResponseHead = "response_head"
	peerSocketFrameBody         = "body"
	peerSocketFrameEnd          = "end"
	peerSocketFrameCancel       = "cancel"

	// peerSocketLargestRequestBody caps one request body carried over a
	// socket, which is sent in one frame.
	peerSocketLargestRequestBody = 64 << 20
	// peerSocketPingInterval and peerSocketReadDeadline keep a socket whose
	// other end vanished from looking connected.
	peerSocketPingInterval = 30 * time.Second
	peerSocketReadDeadline = 90 * time.Second
	// peerSocketStreamBuffer is how many body frames one call may have waiting
	// before its reader is cut off as too slow, so one stalled reader cannot
	// stop every other stream on the socket.
	peerSocketStreamBuffer = 256
	// peerSocketLongestBackoff caps the wait between dial attempts.
	peerSocketLongestBackoff = time.Minute
)

// headersCrossingPeerSockets is every request header a request carried over
// a socket keeps: what the body is, what answer is wanted, where an SSE stream
// resumes, and the peer token. Anything else the other server put in a frame
// is dropped before the handler sees it.
var headersCrossingPeerSockets = []string{"Content-Type", "Accept", "Last-Event-ID", peerTokenHeader}

// peerSocketFrame is one message on a peer socket.
type peerSocketFrame struct {
	Type   string      `json:"type"`
	Stream uint64      `json:"stream"`
	Method string      `json:"method,omitempty"`
	Path   string      `json:"path,omitempty"`
	Header http.Header `json:"header,omitempty"`
	Status int         `json:"status,omitempty"`
	Body   []byte      `json:"body,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// peerSocketConnectionContextKey marks a request that arrived over a peer
// socket with the connection the socket belongs to.
type peerSocketConnectionContextKey struct{}

// peerSocketConnectionOfRequest returns the connection a request arrived over,
// when it arrived over a peer socket.
func peerSocketConnectionOfRequest(r *http.Request) (string, bool) {
	connectionID, ok := r.Context().Value(peerSocketConnectionContextKey{}).(string)
	return connectionID, ok && connectionID != ""
}

// errPeerNotConnected is a call over a dial-out connection whose socket is not
// open now.
var errPeerNotConnected = errors.New("the other server is not connected: its socket is not open")

// peerSocket is one open socket and the streams on it.
type peerSocket struct {
	server       *Server
	connectionID string
	connection   *websocket.Conn
	writeMutex   sync.Mutex
	nextStream   atomic.Uint64
	streamsMutex sync.Mutex
	calls        map[uint64]*peerSocketCall
	serving      map[uint64]context.CancelFunc
	closed       chan struct{}
	closeOnce    sync.Once
}

// peerSocketCall is a request this side sent, waiting for its answer.
type peerSocketCall struct {
	head   chan peerSocketFrame
	frames chan peerSocketFrame
}

// newPeerSocket wraps an open WebSocket. dialing says which side this is, so
// the two sides number their streams apart: the dialing side odd, the
// accepting side even.
func newPeerSocket(server *Server, connectionID string, connection *websocket.Conn, dialing bool) *peerSocket {
	socket := &peerSocket{server: server, connectionID: connectionID, connection: connection,
		calls: map[uint64]*peerSocketCall{}, serving: map[uint64]context.CancelFunc{}, closed: make(chan struct{})}
	if dialing {
		socket.nextStream.Store(1)
	} else {
		socket.nextStream.Store(2)
	}
	return socket
}

func (socket *peerSocket) send(frame peerSocketFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	socket.writeMutex.Lock()
	defer socket.writeMutex.Unlock()
	socket.connection.SetWriteDeadline(time.Now().Add(peerSocketReadDeadline))
	return socket.connection.WriteMessage(websocket.TextMessage, data)
}

func (socket *peerSocket) close() {
	socket.closeOnce.Do(func() {
		close(socket.closed)
		socket.connection.Close()
		socket.streamsMutex.Lock()
		for stream, call := range socket.calls {
			close(call.frames)
			delete(socket.calls, stream)
		}
		for stream, cancel := range socket.serving {
			cancel()
			delete(socket.serving, stream)
		}
		socket.streamsMutex.Unlock()
	})
}

// run reads frames until the socket fails or closes, and pings the other end
// meanwhile. It returns when the socket is done.
func (socket *peerSocket) run() error {
	socket.connection.SetReadDeadline(time.Now().Add(peerSocketReadDeadline))
	socket.connection.SetPongHandler(func(string) error {
		return socket.connection.SetReadDeadline(time.Now().Add(peerSocketReadDeadline))
	})
	go func() {
		ticker := time.NewTicker(peerSocketPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-socket.closed:
				return
			case <-ticker.C:
				socket.writeMutex.Lock()
				err := socket.connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				socket.writeMutex.Unlock()
				if err != nil {
					socket.close()
					return
				}
			}
		}
	}()
	defer socket.close()
	for {
		_, data, err := socket.connection.ReadMessage()
		if err != nil {
			return err
		}
		socket.connection.SetReadDeadline(time.Now().Add(peerSocketReadDeadline))
		var frame peerSocketFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return fmt.Errorf("a frame that is not JSON: %w", err)
		}
		switch frame.Type {
		case peerSocketFrameRequest:
			socket.serveRequest(frame)
		case peerSocketFrameCancel:
			socket.streamsMutex.Lock()
			if cancel, serving := socket.serving[frame.Stream]; serving {
				cancel()
			}
			socket.streamsMutex.Unlock()
		case peerSocketFrameResponseHead, peerSocketFrameBody, peerSocketFrameEnd:
			socket.deliverToCall(frame)
		default:
			return fmt.Errorf("a frame of unknown type %q", frame.Type)
		}
	}
}

// deliverToCall hands an answer frame to the call waiting for it.
func (socket *peerSocket) deliverToCall(frame peerSocketFrame) {
	socket.streamsMutex.Lock()
	call, waiting := socket.calls[frame.Stream]
	socket.streamsMutex.Unlock()
	if !waiting {
		return
	}
	if frame.Type == peerSocketFrameResponseHead {
		select {
		case call.head <- frame:
		default:
		}
		return
	}
	select {
	case call.frames <- frame:
	default:
		log.Printf("[peer-socket] %s: stream %d's reader is too slow; ending it", socket.connectionID, frame.Stream)
		socket.endCall(frame.Stream)
		socket.send(peerSocketFrame{Type: peerSocketFrameCancel, Stream: frame.Stream})
	}
	if frame.Type == peerSocketFrameEnd {
		socket.endCall(frame.Stream)
	}
}

func (socket *peerSocket) endCall(stream uint64) {
	socket.streamsMutex.Lock()
	defer socket.streamsMutex.Unlock()
	if call, waiting := socket.calls[stream]; waiting {
		close(call.frames)
		delete(socket.calls, stream)
	}
}

// roundTrip sends one request to the other server and returns its answer as
// soon as the head arrives; the body streams after. Closing the body before
// the end tells the other side to stop.
func (socket *peerSocket) roundTrip(ctx context.Context, method, path string, header http.Header, body io.Reader) (*http.Response, error) {
	var bodyBytes []byte
	if body != nil {
		read, err := io.ReadAll(io.LimitReader(body, peerSocketLargestRequestBody+1))
		if err != nil {
			return nil, fmt.Errorf("read the request body: %w", err)
		}
		if len(read) > peerSocketLargestRequestBody {
			return nil, fmt.Errorf("a request body over %d bytes cannot cross a peer socket", peerSocketLargestRequestBody)
		}
		bodyBytes = read
	}
	stream := socket.nextStream.Add(2) - 2
	call := &peerSocketCall{head: make(chan peerSocketFrame, 1), frames: make(chan peerSocketFrame, peerSocketStreamBuffer)}
	socket.streamsMutex.Lock()
	select {
	case <-socket.closed:
		socket.streamsMutex.Unlock()
		return nil, errPeerNotConnected
	default:
	}
	socket.calls[stream] = call
	socket.streamsMutex.Unlock()
	if err := socket.send(peerSocketFrame{Type: peerSocketFrameRequest, Stream: stream, Method: method, Path: path, Header: header, Body: bodyBytes}); err != nil {
		socket.endCall(stream)
		return nil, fmt.Errorf("send the request over the socket: %w", err)
	}
	select {
	case head := <-call.head:
		reader := &peerSocketBodyReader{socket: socket, stream: stream, frames: call.frames, ctx: ctx}
		return &http.Response{StatusCode: head.Status, Status: fmt.Sprintf("%d %s", head.Status, http.StatusText(head.Status)), Header: head.Header, Body: reader}, nil
	case <-ctx.Done():
		socket.endCall(stream)
		socket.send(peerSocketFrame{Type: peerSocketFrameCancel, Stream: stream})
		return nil, ctx.Err()
	case <-socket.closed:
		return nil, errPeerNotConnected
	}
}

// peerSocketBodyReader reads one answer's body frames.
type peerSocketBodyReader struct {
	socket  *peerSocket
	stream  uint64
	frames  chan peerSocketFrame
	ctx     context.Context
	pending []byte
	done    bool
	failure error
}

func (reader *peerSocketBodyReader) Read(buffer []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.done {
			if reader.failure != nil {
				return 0, reader.failure
			}
			return 0, io.EOF
		}
		select {
		case frame, open := <-reader.frames:
			if !open {
				reader.done = true
				if reader.failure == nil {
					reader.failure = errors.New("the peer socket closed before the answer ended")
				}
				continue
			}
			if frame.Type == peerSocketFrameEnd {
				reader.done = true
				if frame.Error != "" {
					reader.failure = errors.New(frame.Error)
				} else {
					reader.failure = nil
				}
				continue
			}
			reader.pending = frame.Body
		case <-reader.ctx.Done():
			reader.done = true
			reader.failure = reader.ctx.Err()
		}
	}
	read := copy(buffer, reader.pending)
	reader.pending = reader.pending[read:]
	return read, nil
}

func (reader *peerSocketBodyReader) Close() error {
	if !reader.done {
		reader.done = true
		reader.socket.endCall(reader.stream)
		reader.socket.send(peerSocketFrame{Type: peerSocketFrameCancel, Stream: reader.stream})
	}
	return nil
}

// serveRequest runs a request the other server sent through this server's
// own ServeHTTP, streaming the answer back as frames.
func (socket *peerSocket) serveRequest(frame peerSocketFrame) {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), peerSocketConnectionContextKey{}, socket.connectionID))
	socket.streamsMutex.Lock()
	socket.serving[frame.Stream] = cancel
	socket.streamsMutex.Unlock()
	go func() {
		defer func() {
			cancel()
			socket.streamsMutex.Lock()
			delete(socket.serving, frame.Stream)
			socket.streamsMutex.Unlock()
		}()
		writer := &peerSocketResponseWriter{socket: socket, stream: frame.Stream, header: http.Header{}}
		request, err := requestCarriedOverPeerSocket(ctx, frame)
		if err != nil {
			writeJSONError(writer, http.StatusBadRequest, "invalid_peer_socket_request", err.Error())
		} else {
			socket.server.ServeHTTP(writer, request)
		}
		writer.finish()
	}()
}

// requestCarriedOverPeerSocket builds the request a frame describes, keeping
// only the headers that may cross.
func requestCarriedOverPeerSocket(ctx context.Context, frame peerSocketFrame) (*http.Request, error) {
	if !strings.HasPrefix(frame.Path, "/") {
		return nil, fmt.Errorf("path %q must start with /", frame.Path)
	}
	target, err := url.ParseRequestURI(frame.Path)
	if err != nil {
		return nil, fmt.Errorf("path %q: %w", frame.Path, err)
	}
	request, err := http.NewRequestWithContext(ctx, frame.Method, "http://peer-socket"+target.RequestURI(), bytes.NewReader(frame.Body))
	if err != nil {
		return nil, err
	}
	request.ContentLength = int64(len(frame.Body))
	request.RemoteAddr = "peer-socket"
	request.RequestURI = target.RequestURI()
	for _, name := range headersCrossingPeerSockets {
		if values := frame.Header.Values(name); len(values) > 0 {
			request.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	return request, nil
}

// peerSocketResponseWriter sends a handler's answer back as frames.
type peerSocketResponseWriter struct {
	socket      *peerSocket
	stream      uint64
	header      http.Header
	wroteHead   bool
	sendFailure error
}

func (writer *peerSocketResponseWriter) Header() http.Header { return writer.header }

func (writer *peerSocketResponseWriter) WriteHeader(status int) {
	if writer.wroteHead {
		return
	}
	writer.wroteHead = true
	header := writer.header.Clone()
	header.Del("Set-Cookie")
	writer.sendFailure = writer.socket.send(peerSocketFrame{Type: peerSocketFrameResponseHead, Stream: writer.stream, Status: status, Header: header})
}

func (writer *peerSocketResponseWriter) Write(data []byte) (int, error) {
	if !writer.wroteHead {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.sendFailure != nil {
		return 0, writer.sendFailure
	}
	if len(data) == 0 {
		return 0, nil
	}
	if err := writer.socket.send(peerSocketFrame{Type: peerSocketFrameBody, Stream: writer.stream, Body: append([]byte(nil), data...)}); err != nil {
		writer.sendFailure = err
		return 0, err
	}
	return len(data), nil
}

// Flush does nothing: every Write is already sent as its own frame.
func (writer *peerSocketResponseWriter) Flush() {}

func (writer *peerSocketResponseWriter) finish() {
	if !writer.wroteHead {
		writer.WriteHeader(http.StatusOK)
	}
	writer.socket.send(peerSocketFrame{Type: peerSocketFrameEnd, Stream: writer.stream})
}

// peerSocketRegistry is every open socket and every dialer this server runs.
type peerSocketRegistry struct {
	mutex   sync.Mutex
	sockets map[string]*peerSocket
	dialers map[string]context.CancelFunc
}

func newPeerSocketRegistry() *peerSocketRegistry {
	return &peerSocketRegistry{sockets: map[string]*peerSocket{}, dialers: map[string]context.CancelFunc{}}
}

// open records a socket for its connection, closing any older one.
func (registry *peerSocketRegistry) open(socket *peerSocket) {
	registry.mutex.Lock()
	previous := registry.sockets[socket.connectionID]
	registry.sockets[socket.connectionID] = socket
	registry.mutex.Unlock()
	if previous != nil {
		previous.close()
	}
}

// closed forgets a socket that ended, unless a newer one replaced it.
func (registry *peerSocketRegistry) closed(socket *peerSocket) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	if registry.sockets[socket.connectionID] == socket {
		delete(registry.sockets, socket.connectionID)
	}
}

func (registry *peerSocketRegistry) socketOf(connectionID string) *peerSocket {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	return registry.sockets[connectionID]
}

// stop ends a connection's dialer and closes its socket.
func (registry *peerSocketRegistry) stop(connectionID string) {
	registry.mutex.Lock()
	cancel := registry.dialers[connectionID]
	delete(registry.dialers, connectionID)
	socket := registry.sockets[connectionID]
	delete(registry.sockets, connectionID)
	registry.mutex.Unlock()
	if cancel != nil {
		cancel()
	}
	if socket != nil {
		socket.close()
	}
}

// peerSocketDialer dials other servers' sockets. It verifies certificates
// with the system's roots, which is gorilla's default and is never switched
// off here; tests swap it for one that trusts their own certificate.
var peerSocketDialer = &websocket.Dialer{HandshakeTimeout: 20 * time.Second}

// StartPeerDialers starts a dialer for every live dial-out connection this
// server initiated, so a restart reconnects them. Called once from main.
func (s *Server) StartPeerDialers() {
	connections, err := s.store.ListPeerConnections()
	if err != nil {
		log.Printf("[peer-socket] ERROR: list peer connections to dial: %v", err)
		return
	}
	for _, connection := range connections {
		if connection.RemovedAt == nil && thisServerDials(connection) {
			s.startPeerDialer(connection)
		}
	}
}

// thisServerDials says whether this server holds the connection's socket.
func thisServerDials(connection store.PeerConnection) bool {
	return connection.Transport == msg.PeerTransportDialOut && connection.Role == msg.PeerConnectionRoleInitiating
}

// startPeerDialer keeps a socket open to the other server until the
// connection is removed, backing off between failed attempts.
func (s *Server) startPeerDialer(connection store.PeerConnection) {
	ctx, cancel := context.WithCancel(context.Background())
	s.peerSockets.mutex.Lock()
	if previous := s.peerSockets.dialers[connection.ID]; previous != nil {
		previous()
	}
	s.peerSockets.dialers[connection.ID] = cancel
	s.peerSockets.mutex.Unlock()
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			started := time.Now()
			err := s.dialPeerSocketOnce(ctx, connection)
			if ctx.Err() != nil {
				return
			}
			if time.Since(started) > peerSocketLongestBackoff {
				backoff = time.Second
			}
			log.Printf("[peer-socket] %s (%s): socket ended: %v; dialing again in %s", connection.ID, connection.DisplayName, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, peerSocketLongestBackoff)
		}
	}()
}

// dialPeerSocketOnce opens one socket and serves it until it ends.
func (s *Server) dialPeerSocketOnce(ctx context.Context, connection store.PeerConnection) error {
	credential, err := s.outboundConnectionCredential(ctx, connection)
	if err != nil {
		return err
	}
	socketURL, err := url.Parse(connection.RemoteBaseURL + peerSocketPath)
	if err != nil {
		return err
	}
	socketURL.Scheme = "wss"
	header := http.Header{}
	header.Set(peerConnectionCredentialHeader, credential)
	conn, response, err := peerSocketDialer.DialContext(ctx, socketURL.String(), header)
	if err != nil {
		if response != nil {
			answer, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			return fmt.Errorf("dial %s: %s: %s", socketURL, response.Status, strings.TrimSpace(string(answer)))
		}
		return fmt.Errorf("dial %s: %w", socketURL, err)
	}
	socket := newPeerSocket(s, connection.ID, conn, true)
	s.peerSockets.open(socket)
	defer s.peerSockets.closed(socket)
	log.Printf("[peer-socket] %s (%s): connected to %s", connection.ID, connection.DisplayName, socketURL)
	go func() {
		select {
		case <-ctx.Done():
			socket.close()
		case <-socket.closed:
		}
	}()
	return socket.run()
}

var peerSocketUpgrader = websocket.Upgrader{
	// The other server is not a browser, and its credential is checked
	// before the upgrade; there is no origin to hold it to.
	CheckOrigin: func(*http.Request) bool { return true },
}

// handlePeerSocket: GET /peer/socket. The server without an address opens
// its connection's socket here, presenting the connection's credential.
func (s *Server) handlePeerSocket(w http.ResponseWriter, r *http.Request) {
	if s.refuseWhileDemoLoginIsOn(w, "a peer socket") {
		return
	}
	connection, credentialError := s.connectionPresentingCredential(r)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	if connection.Transport != msg.PeerTransportDialOut || connection.Role != msg.PeerConnectionRoleAccepting {
		writeJSONError(w, http.StatusConflict, "peer_connection_has_no_socket", fmt.Sprintf(
			"%s is %s and this server %s it, so no socket is opened to this server for it", connection.ID, connection.Transport, connection.Role))
		return
	}
	conn, err := peerSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[peer-socket] %s: upgrade: %v", connection.ID, err)
		return
	}
	socket := newPeerSocket(s, connection.ID, conn, false)
	s.peerSockets.open(socket)
	defer s.peerSockets.closed(socket)
	log.Printf("[peer-socket] %s (%s): connected", connection.ID, connection.DisplayName)
	err = socket.run()
	log.Printf("[peer-socket] %s (%s): socket ended: %v", connection.ID, connection.DisplayName, err)
}
