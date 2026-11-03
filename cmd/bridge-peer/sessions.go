package main

// Running sessions on a connected server, through this server's connection
// proxy (/peer-connections/{id}/proxy/<route>), so an agent here can start a
// session there, or continue one, and read its reply with one command.
//
// The proxy refuses a session posting token, so these calls sign in with the
// session agent token (LLM_BRIDGE_PRINCIPAL_TOKEN), which this server accepts
// off the store proxies only for an administrator's session; the other server
// then runs the session as the principal that administrator is mapped to
// there. Ids in every answer are the other server's.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/textutil"
	"github.com/kayushkin/llm-bridge/msg"
)

const principalTokenEnvironmentVariable = "LLM_BRIDGE_PRINCIPAL_TOKEN"

// proxyPath is the route on this server that reaches route on the other one.
func proxyPath(connection connectionView, route string) string {
	return "/peer-connections/" + url.PathEscape(connection.Connection.ID) + "/proxy" + route
}

// proxyRequest builds a request to a route on the other server, signed in as
// this session's principal (or as the service token's administrator).
func (c *client) proxyRequest(ctx context.Context, method string, connection connectionView, route string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(data))
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+proxyPath(connection, route), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.serviceToken != "":
		request.Header.Set(serviceTokenHeader, c.serviceToken)
		request.Header.Set(principalIdentityHeader, c.asPrincipal)
	case c.principalToken != "":
		request.Header.Set("Authorization", "Bearer "+c.principalToken)
	default:
		return nil, fmt.Errorf("no credential for another server's sessions: this session has no %s (it runs with no principal), and the connection proxy does not take %s",
			principalTokenEnvironmentVariable, postingTokenEnvironmentVariable)
	}
	return request, nil
}

// proxyCall sends one request to the other server and decodes a JSON answer
// of the wanted status into into, or returns the answer's own text.
func (c *client) proxyCall(ctx context.Context, method string, connection connectionView, route string, body any, wantStatus int, into any) error {
	request, err := c.proxyRequest(ctx, method, connection, route, body)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%s %s on %s: %d %s", method, route, connection.name(), response.StatusCode, strings.TrimSpace(string(answer)))
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			return fmt.Errorf("%s %s on %s: decode: %w", method, route, connection.name(), err)
		}
	}
	return nil
}

// instanceView is the part of an instance record listInstances prints.
type instanceView struct {
	ID          string `json:"id"`
	HarnessType string `json:"harness_type"`
	Name        string `json:"name"`
}

func listInstances(ctx context.Context, c *client, out io.Writer, connection connectionView) error {
	var instances []instanceView
	if err := c.proxyCall(ctx, http.MethodGet, connection, "/instances", nil, http.StatusOK, &instances); err != nil {
		return err
	}
	if len(instances) == 0 {
		fmt.Fprintf(out, "%s lists no instances you may start sessions on\n", connection.name())
		return nil
	}
	for _, instance := range instances {
		fmt.Fprintf(out, "%s  %s  %s\n", instance.ID, instance.HarnessType, instance.Name)
	}
	return nil
}

// runOnServer sends message to a session on the other server — a new one on
// instanceID, or sessionID when given — waits for that turn's result and
// prints its text. The session is left running there, so the operator sees
// it on that server and a later run can continue it with --session.
func runOnServer(ctx context.Context, c *client, out, progress io.Writer, connection connectionView, instanceID, sessionID, message string) error {
	if sessionID == "" {
		created, err := c.createSessionOnServer(ctx, connection, instanceID, message)
		if err != nil {
			return err
		}
		sessionID = created
	}
	fmt.Fprintf(progress, "[bridge-peer] session %s on %s\n", sessionID, connection.name())
	turn, err := newClientRequestID()
	if err != nil {
		return err
	}
	var sent struct {
		MessageID string `json:"message_id"`
	}
	route := "/sessions/" + url.PathEscape(sessionID)
	if err := c.proxyCall(ctx, http.MethodPost, connection, route+"/send", msg.SendMessageRequest{Message: message, ClientRequestID: turn}, http.StatusOK, &sent); err != nil {
		return err
	}
	result, err := c.waitForTurnResult(ctx, progress, connection, sessionID, turn)
	if err != nil {
		return fmt.Errorf("%w (session %s on %s; run again with --session %s to send it more)", err, sessionID, connection.name(), sessionID)
	}
	fmt.Fprintln(out, result.Text)
	if result.IsError {
		return fmt.Errorf("the turn ended in an error (session %s on %s)", sessionID, connection.name())
	}
	return nil
}

// createSessionOnServer creates an idle session on the other server. The
// session belongs to whoever the other server maps this caller to; its
// manager is not named, because the other server has no record of this one's
// sessions.
func (c *client) createSessionOnServer(ctx context.Context, connection connectionView, instanceID, message string) (string, error) {
	if instanceID == "" {
		return "", fmt.Errorf("pass --instance with one of %s's instance ids (bridge-peer instances %s), or --session to continue a session there", connection.name(), connection.name())
	}
	// GET /instances/{id} is an operator route, so the instance is found in
	// the list the caller may start sessions on.
	var instances []instanceView
	if err := c.proxyCall(ctx, http.MethodGet, connection, "/instances", nil, http.StatusOK, &instances); err != nil {
		return "", err
	}
	var harness msg.Harness
	for _, instance := range instances {
		if instance.ID == instanceID {
			harness = msg.Harness(instance.HarnessType)
		}
	}
	if harness == "" {
		return "", fmt.Errorf("%s lists no instance %s that you may start sessions on (bridge-peer instances %s)", connection.name(), instanceID, connection.name())
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	request := msg.CreateSessionRequest{
		Harness:     harness,
		InstanceID:  instanceID,
		DisplayName: sessionDisplayName(c.thisServerName(), message),
		Type:        msg.SessionTypeAutonomous,
		Purpose:     msg.PurposeDelegate,
		Origin:      "bridge-peer",
	}
	if err := c.proxyCall(ctx, http.MethodPost, connection, "/sessions", request, http.StatusCreated, &created); err != nil {
		return "", err
	}
	if created.SessionID == "" {
		return "", fmt.Errorf("%s created a session but named no session_id", connection.name())
	}
	return created.SessionID, nil
}

// thisServerName is this server's own server.name, or empty when it has none
// or cannot say.
func (c *client) thisServerName() string {
	var description struct {
		Name string `json:"name"`
	}
	if err := c.call(http.MethodGet, "/server", nil, http.StatusOK, &description); err != nil {
		return ""
	}
	return description.Name
}

// sessionDisplayName says which server asked and what it asked.
func sessionDisplayName(fromServer, message string) string {
	const longestPrompt = 70
	prompt := strings.Join(strings.Fields(message), " ")
	if cut := textutil.TruncateToRuneLimit(prompt, longestPrompt); cut != prompt {
		prompt = cut + "…"
	}
	if fromServer == "" {
		return "from a connected server: " + prompt
	}
	return "from " + fromServer + ": " + prompt
}

func newClientRequestID() (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "bridge-peer-" + hex.EncodeToString(random), nil
}

// waitForTurnResult reads the session's event stream until the result of the
// turn stamped clientRequestID. The stream crosses the connection's socket,
// which can drop; it is then opened again from the last event seen, so a drop
// costs a reconnect, not the answer.
func (c *client) waitForTurnResult(ctx context.Context, progress io.Writer, connection connectionView, sessionID, clientRequestID string) (*msg.ResultEvent, error) {
	lastEventID := ""
	for {
		result, seen, err := c.readEventsUntilResult(ctx, connection, sessionID, clientRequestID, lastEventID)
		if seen != "" {
			lastEventID = seen
		}
		if result != nil || ctx.Err() != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("gave up waiting for the reply: %w", ctx.Err())
			}
			return result, nil
		}
		var permanent *errStreamRefused
		if errors.As(err, &permanent) {
			return nil, err
		}
		fmt.Fprintf(progress, "[bridge-peer] the event stream ended (%v); opening it again\n", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gave up waiting for the reply: %w", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

// errStreamRefused is an answer that opening the stream again will not change.
type errStreamRefused struct{ text string }

func (err *errStreamRefused) Error() string { return err.text }

// readEventsUntilResult opens the event stream once. It returns the turn's
// result when it arrives, and always the id of the last event it read.
func (c *client) readEventsUntilResult(ctx context.Context, connection connectionView, sessionID, clientRequestID, lastEventID string) (*msg.ResultEvent, string, error) {
	request, err := c.proxyRequest(ctx, http.MethodGet, connection, "/sessions/"+url.PathEscape(sessionID)+"/events", nil)
	if err != nil {
		return nil, "", &errStreamRefused{err.Error()}
	}
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	streaming := &http.Client{Transport: c.http.Transport}
	response, err := streaming.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		text := fmt.Sprintf("open the event stream of %s on %s: %d %s", sessionID, connection.name(), response.StatusCode, strings.TrimSpace(string(answer)))
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			return nil, "", errors.New(text)
		}
		return nil, "", &errStreamRefused{text}
	}
	seen := ""
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if id, isID := strings.CutPrefix(line, "id:"); isID {
			seen = strings.TrimSpace(id)
			continue
		}
		payload, isData := strings.CutPrefix(line, "data:")
		if !isData {
			continue
		}
		var event msg.Event
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &event) != nil || event.ClientRequestID != clientRequestID {
			continue
		}
		switch event.Type {
		case msg.EventResult:
			if event.Result == nil {
				return nil, seen, &errStreamRefused{"the other server sent a result with no result payload"}
			}
			return event.Result, seen, nil
		case msg.EventError:
			text := "the session reported an error with no message"
			if event.Error != nil && event.Error.Message != "" {
				text = event.Error.Message
			}
			return nil, seen, &errStreamRefused{"the session failed: " + text}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, seen, err
	}
	return nil, seen, errors.New("the stream closed before the reply")
}

// principalTokenOfThisSession is the session agent token, empty when this
// session runs with no principal.
func principalTokenOfThisSession() string {
	return os.Getenv(principalTokenEnvironmentVariable)
}
