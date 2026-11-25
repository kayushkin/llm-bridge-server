package rulestore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LLMBridge reads sessions from llm-bridge-server, as the internal service.
type LLMBridge struct {
	BaseURL      string
	ServiceToken string
	// Client reads the full list; Stream follows the feed and has no timeout,
	// because a feed is a request that is meant never to end.
	Client *http.Client
	Stream *http.Client
}

// NewLLMBridge builds the reader. Both values are required settings.
func NewLLMBridge(baseURL, serviceToken string) *LLMBridge {
	return &LLMBridge{BaseURL: baseURL, ServiceToken: serviceToken,
		Client: &http.Client{Timeout: 2 * time.Minute}, Stream: &http.Client{}}
}

func (b *LLMBridge) request(ctx context.Context, path string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-LLM-Bridge-Service-Token", b.ServiceToken)
	return request, nil
}

// ListSessions reads every session, each verbatim.
func (b *LLMBridge) ListSessions(ctx context.Context) ([]json.RawMessage, error) {
	request, err := b.request(ctx, "/sessions")
	if err != nil {
		return nil, err
	}
	response, err := b.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("llm-bridge-server GET /sessions: %d %.300s", response.StatusCode, body)
	}
	var sessions []json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("read GET /sessions: %w", err)
	}
	return sessions, nil
}

// GetSession reads one session from the bridge. found is false on a 404.
func (b *LLMBridge) GetSession(ctx context.Context, sessionID string) (SessionSnapshot, bool, error) {
	var session SessionSnapshot
	request, err := b.request(ctx, "/sessions/"+url.PathEscape(sessionID))
	if err != nil {
		return session, false, err
	}
	response, err := b.Client.Do(request)
	if err != nil {
		return session, false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return session, false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return session, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return session, false, fmt.Errorf("llm-bridge-server GET /sessions/%s: %d %.300s", sessionID, response.StatusCode, body)
	}
	session, err = ParseSessionSnapshot(body)
	return session, true, err
}

// FeedHello is the first frame of every connection to /session-events.
type FeedHello struct {
	StreamID    string `json:"stream_id"`
	Resume      string `json:"resume"` // none | replayed | gap
	LastEventID string `json:"last_event_id"`
}

// FeedHandler receives the feed's frames in order.
type FeedHandler struct {
	Hello  func(FeedHello) error
	Upsert func(session json.RawMessage) error
	Delete func(sessionID string) error
	// Unknown is told about a frame type this reader does not handle.
	Unknown func(event string) error
}

// FollowSessions reads /session-events until the stream ends or a handler
// fails, and returns the id of the last frame it handled, to resume from.
func (b *LLMBridge) FollowSessions(ctx context.Context, lastEventID string, handler FeedHandler) (string, error) {
	request, err := b.request(ctx, "/session-events")
	if err != nil {
		return lastEventID, err
	}
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := b.Stream.Do(request)
	if err != nil {
		return lastEventID, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return lastEventID, fmt.Errorf("llm-bridge-server GET /session-events: %d %.300s", response.StatusCode, body)
	}
	reader := bufio.NewReaderSize(response.Body, 1<<20)
	var id, event string
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lastEventID, fmt.Errorf("feed ended: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event == "" && data.Len() == 0 {
				continue
			}
			if err := dispatchFrame(event, data.String(), handler); err != nil {
				return lastEventID, err
			}
			if id != "" {
				lastEventID = id
			}
			id, event = "", ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// keepalive comment
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

func dispatchFrame(event, data string, handler FeedHandler) error {
	switch event {
	case "hello":
		var hello FeedHello
		if err := json.Unmarshal([]byte(data), &hello); err != nil {
			return fmt.Errorf("read hello frame: %w", err)
		}
		return handler.Hello(hello)
	case "upsert":
		var frame struct {
			Session json.RawMessage `json:"session"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil || len(frame.Session) == 0 {
			return fmt.Errorf("read upsert frame %.200s: %v", data, err)
		}
		return handler.Upsert(frame.Session)
	case "delete":
		var frame struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil || frame.SessionID == "" {
			return fmt.Errorf("read delete frame %.200s: %v", data, err)
		}
		return handler.Delete(frame.SessionID)
	case "signal":
		// A session's open questions or notifications changed. Not a change
		// to the session row, so there is nothing to decide.
		return nil
	}
	// A frame type this reader does not know is new upstream. Dropping the
	// connection over it would reconnect, meet it again and loop — which is
	// what happened when the "signal" frame was first met on 2026-10-20, 11,201
	// times. So it is skipped, and counted and named in GET /health.
	return handler.Unknown(event)
}
