// Package filestore is mailstack's client for file-store, which owns uploaded
// bytes. mailstack owns which file hangs on which message and revision, and who
// may read it; file-store owns the file. So this client carries bytes and
// file-store's own answers through unchanged, and re-describes neither.
//
// It is the same shape as kanban-store's internal/filestore, which cannot be
// imported from here.
package filestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OwnerService is how mailstack names itself as the owner of a file.
const OwnerService = "mailstack"

// ServiceTokenHeader is what file-store asks of every caller.
const ServiceTokenHeader = "X-File-Store-Service-Token"

// File is file-store's record. Only the fields mailstack reads are named.
type File struct {
	ID          string `json:"id"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
}

// RefusalError is file-store saying no: its status and its body, to relay.
type RefusalError struct {
	Status int
	Body   []byte
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("file-store refused: %d %s", e.Status, strings.TrimSpace(string(e.Body)))
}

// ErrNotFound is file-store not having that file.
var ErrNotFound = errors.New("file-store has no such file")

type Client struct {
	BaseURL string
	Token   string
	// HTTP has no overall timeout: an upload or a download takes as long as
	// its bytes do. The header timeout bounds a file-store that does not answer.
	HTTP *http.Client
}

func New(baseURL, token string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Transport: transport}}
}

func (c *Client) do(request *http.Request) (*http.Response, error) {
	request.Header.Set(ServiceTokenHeader, c.Token)
	return c.HTTP.Do(request)
}

// Upload stores content as a file of ownerRef's.
func (c *Client) Upload(content []byte, contentType, filename, ownerRef string) (*File, error) {
	query := url.Values{"filename": {filename}, "owner_service": {OwnerService}, "owner_ref": {ownerRef}}
	request, err := http.NewRequest(http.MethodPost, c.BaseURL+"/files?"+query.Encode(), bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	request.ContentLength = int64(len(content))
	request.Header.Set("Content-Type", contentType)
	response, err := c.do(request)
	if err != nil {
		return nil, fmt.Errorf("upload %s to file-store: %w", filename, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read file-store's answer to uploading %s: %w", filename, err)
	}
	if response.StatusCode != http.StatusCreated {
		return nil, &RefusalError{Status: response.StatusCode, Body: body}
	}
	var file File
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("file-store answered a file that does not parse: %w", err)
	}
	return &file, nil
}

// Content opens a file's bytes. The response is file-store's own — status,
// headers and body — for the caller to pass on and close. forwarded carries
// the reader's Range and conditional headers.
func (c *Client) Content(fileID string, inline bool, forwarded http.Header) (*http.Response, error) {
	target := c.BaseURL + "/files/" + url.PathEscape(fileID) + "/content"
	if inline {
		target += "?inline=true"
	}
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := forwarded.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	return c.do(request)
}

// Purge destroys a file. A file file-store does not have is ErrNotFound, which
// a caller purging what it owns may take as done.
func (c *Client) Purge(fileID string) error {
	request, err := http.NewRequest(http.MethodDelete, c.BaseURL+"/files/"+url.PathEscape(fileID)+"?hard=true", nil)
	if err != nil {
		return err
	}
	response, err := c.do(request)
	if err != nil {
		return fmt.Errorf("purge %s from file-store: %w", fileID, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return &RefusalError{Status: response.StatusCode, Body: body}
	}
}
