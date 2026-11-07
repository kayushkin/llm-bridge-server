// Package storeclient calls this host's JSON stores. A store's answer passes
// through unchanged; a status other than the one expected is an error that
// carries the status, so a caller can tell a conflict from a failure.
package storeclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// StatusError is an answer with an unexpected status.
type StatusError struct {
	Method, URL string
	Status      int
	Body        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.Status, e.Body)
}

// IsStatus reports whether err is an answer with status.
func IsStatus(err error, status int) bool {
	var statusError *StatusError
	return errors.As(err, &statusError) && statusError.Status == status
}

var client = &http.Client{Timeout: 2 * time.Minute}

// Do sends body (nil for none) as JSON and decodes a 2xx answer into into
// (nil to ignore it).
func Do(method, url string, body, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &StatusError{Method: method, URL: url, Status: response.StatusCode, Body: string(bytes.TrimSpace(answer))}
	}
	if into == nil || len(answer) == 0 {
		return nil
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return fmt.Errorf("%s %s answered something unreadable: %w", method, url, err)
	}
	return nil
}

// Get decodes a GET's answer into into.
func Get(url string, into any) error { return Do(http.MethodGet, url, nil, into) }
