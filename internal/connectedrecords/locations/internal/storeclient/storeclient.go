// Package storeclient makes the plain JSON calls the location-store commands
// other than the importers make.
package storeclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one location-store.
type Client struct {
	URL  string
	http *http.Client
}

// New makes a client. Grouping a year of history takes seconds, so the
// timeout is generous.
func New(url string) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), http: &http.Client{Timeout: 10 * time.Minute}}
}

// Do sends body (nil for none) and decodes a 200 answer into answer.
func (c *Client) Do(method, path string, body, answer any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, c.URL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: location-store answered %d: %s", method, path, response.StatusCode, bytes.TrimSpace(content))
	}
	return json.Unmarshal(content, answer)
}
