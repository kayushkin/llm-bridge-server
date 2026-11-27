package localtools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools/schema"
)

// DefaultPinchtabURL is where PinchTab answers when nothing says otherwise.
const DefaultPinchtabURL = "http://localhost:9867"

// PinchtabConnection is where the browser tool finds PinchTab and what it
// presents there. The tool does not read the environment: whoever builds it
// says where PinchTab is, so a service can declare the two values as settings
// and a command-line program can read them however it likes.
type PinchtabConnection struct {
	// BaseURL is PinchTab's address. Empty means DefaultPinchtabURL.
	BaseURL string
	// Token is sent as a bearer token when it is not empty.
	Token string
}

func (connection PinchtabConnection) baseURL() string {
	if connection.BaseURL == "" {
		return DefaultPinchtabURL
	}
	return strings.TrimRight(connection.BaseURL, "/")
}

// escapePathSegment percent-encodes a value so it occupies exactly one path
// segment on the wire.
//
// Both values this guards reach the path from outside this process. tab_id is a
// field on the browser tool's own input struct, so the model fills it; the
// instance id comes back in PinchTab's answer to GET /instances. Concatenated
// raw, a value carrying "/", "?" or "#" addresses a different PinchTab endpoint
// than the one the action asked for, and does it silently: net/http strips a
// fragment before the request leaves, and a server routes on the path alone, so
// the wrong request comes back with a perfectly ordinary answer.
//
// PathEscape is a no-op for every well-formed PinchTab id, so no legitimate
// call changes shape.
func escapePathSegment(segment string) string {
	return url.PathEscape(segment)
}

// pinchtabStatusError reports a PinchTab response whose status was not 2xx. It
// carries the status separately from the body so a caller — or a test — can ask
// what PinchTab actually said rather than matching on a formatted string.
type pinchtabStatusError struct {
	StatusCode int
	Body       string
}

func (e *pinchtabStatusError) Error() string {
	return fmt.Sprintf("pinchtab returned %d: %s", e.StatusCode, e.Body)
}

// requestExpectingSuccess sends one request to PinchTab and returns the
// response body only when the status is 2xx. A non-2xx is an error, never a
// body.
//
// It used to be pinchtabDo, which never read resp.StatusCode: a 404, a 401 or a
// 500 came back with err == nil and every action handed PinchTab's error body to
// the model as the result of the action. `navigate` to a URL PinchTab rejected
// read as a navigation that worked, and `tabs` decoded an error body to zero
// instances and reported "no instances running" — which is exactly what a
// browser with no tabs open looks like.
//
// It also used http.NewRequest, so no caller's context reached the request and
// canceling a browser tool call canceled nothing.
func (connection PinchtabConnection) requestExpectingSuccess(ctx context.Context, method, path string, body any) ([]byte, error) {
	base := connection.baseURL()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if connection.Token != "" {
		req.Header.Set("Authorization", "Bearer "+connection.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &pinchtabStatusError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// Browser returns a tool that controls a browser via PinchTab's HTTP API at
// connection.
func Browser(connection PinchtabConnection) Impl {
	type input struct {
		Action string `json:"action"`
		URL    string `json:"url"`
		Ref    string `json:"ref"`
		Text   string `json:"text"`
		TabID  string `json:"tab_id"`
		Filter string `json:"filter"`
	}
	return Impl{
		Name: "browser",
		Description: `Control a browser via PinchTab. Actions:
- navigate: go to a URL (requires "url")
- snapshot: get accessibility tree with element refs (e0, e1...). Use filter="interactive" for clickable elements only.
- click: click element by ref (requires "ref")
- type: type text into element (requires "ref" and "text")
- text: extract page text content
- screenshot: take a screenshot (returns base64 JPEG)
- tabs: list open tabs
- close: close a tab by tab_id`,
		InputSchema: schema.Props([]string{"action"}, map[string]any{
			"action": schema.Str(`Action: navigate, snapshot, click, type, text, screenshot, tabs, close`),
			"url":    schema.Str("URL to navigate to (for navigate action)"),
			"ref":    schema.Str("Element ref like e0, e5 (for click/type actions)"),
			"text":   schema.Str("Text to type (for type action)"),
			"tab_id": schema.Str("Tab ID (for close action)"),
			"filter": schema.Str("Snapshot filter: 'interactive' for clickable elements only"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			switch in.Action {
			case "navigate":
				if in.URL == "" {
					return "error: url is required for navigate", nil
				}
				data, err := connection.requestExpectingSuccess(ctx, "POST", "/navigate", map[string]string{"url": in.URL})
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			case "snapshot":
				// url.Values, not escapePathSegment: this value sits in the
				// query, where "&" and "=" are the separators. PathEscape
				// leaves both alone, so it would read as a repair and still
				// let a filter add a parameter PinchTab was never asked for.
				path := "/snapshot"
				if in.Filter != "" {
					path += "?" + url.Values{"filter": {in.Filter}}.Encode()
				}
				data, err := connection.requestExpectingSuccess(ctx, "GET", path, nil)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			case "click":
				if in.Ref == "" {
					return "error: ref is required for click", nil
				}
				data, err := connection.requestExpectingSuccess(ctx, "POST", "/action", map[string]string{"kind": "click", "ref": in.Ref})
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			case "type":
				if in.Ref == "" {
					return "error: ref is required for type", nil
				}
				data, err := connection.requestExpectingSuccess(ctx, "POST", "/action", map[string]string{"kind": "type", "ref": in.Ref, "text": in.Text})
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			case "text":
				data, err := connection.requestExpectingSuccess(ctx, "GET", "/text", nil)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			case "screenshot":
				data, err := connection.requestExpectingSuccess(ctx, "GET", "/screenshot", nil)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data), nil

			case "tabs":
				instData, err := connection.requestExpectingSuccess(ctx, "GET", "/instances", nil)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				var instances []struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(instData, &instances); err != nil {
					return fmt.Sprintf("error parsing instances: %s", err), nil
				}
				if len(instances) == 0 {
					return "no instances running", nil
				}
				data, err := connection.requestExpectingSuccess(ctx, "GET", "/instances/"+escapePathSegment(instances[0].ID)+"/tabs", nil)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			case "close":
				if in.TabID == "" {
					return "error: tab_id is required for close", nil
				}
				data, err := connection.requestExpectingSuccess(ctx, "POST", "/tabs/"+escapePathSegment(in.TabID)+"/close", nil)
				if err != nil {
					return fmt.Sprintf("error: %s", err), nil
				}
				return string(data), nil

			default:
				return fmt.Sprintf("error: unknown action %q. Use: navigate, snapshot, click, type, text, screenshot, tabs, close", in.Action), nil
			}
		},
	}
}
