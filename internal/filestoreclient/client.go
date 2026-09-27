// Package filestoreclient is llm-bridge-server's client for file-store, which
// owns the bytes of every file shared into a session. This server owns which
// session a file belongs to and who may reach it; file-store owns the file. So
// the client carries bytes and file-store's own answers through unchanged.
package filestoreclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OwnerService is how this server names itself as the owner of a file.
const OwnerService = "llm-bridge-server"

// ServiceTokenHeader is what file-store asks of every caller.
const ServiceTokenHeader = "X-File-Store-Service-Token"

// File is the part of file-store's record this server reads.
type File struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
}

// Limits is file-store's GET /limits.
type Limits struct {
	MaximumFileBytes int64 `json:"maximum_file_bytes"`
}

// RefusalError is file-store saying no: its status and its body, to relay.
type RefusalError struct {
	Status int
	Body   []byte
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("file-store refused: %d %s", e.Status, strings.TrimSpace(string(e.Body)))
}

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

// Limits reads the largest file file-store takes, so an upload can be refused
// before its bytes are spooled to disk rather than after.
func (c *Client) Limits() (Limits, error) {
	request, err := http.NewRequest(http.MethodGet, c.BaseURL+"/limits", nil)
	if err != nil {
		return Limits{}, err
	}
	response, err := c.do(request)
	if err != nil {
		return Limits{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Limits{}, err
	}
	if response.StatusCode != http.StatusOK {
		return Limits{}, &RefusalError{Status: response.StatusCode, Body: body}
	}
	var limits Limits
	if err := json.Unmarshal(body, &limits); err != nil {
		return Limits{}, fmt.Errorf("file-store answered limits that do not parse: %w", err)
	}
	return limits, nil
}

// Upload streams content to file-store as a file of sessionID's.
func (c *Client) Upload(content io.Reader, contentLength int64, contentType, filename, sessionID string) (File, error) {
	query := url.Values{"filename": {filename}, "owner_service": {OwnerService}, "owner_ref": {sessionID}}
	request, err := http.NewRequest(http.MethodPost, c.BaseURL+"/files?"+query.Encode(), content)
	if err != nil {
		return File{}, err
	}
	request.ContentLength = contentLength
	request.Header.Set("Content-Type", contentType)
	response, err := c.do(request)
	if err != nil {
		return File{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return File{}, err
	}
	if response.StatusCode != http.StatusCreated {
		return File{}, &RefusalError{Status: response.StatusCode, Body: body}
	}
	var file File
	if err := json.Unmarshal(body, &file); err != nil {
		return File{}, fmt.Errorf("file-store answered a file that does not parse: %w", err)
	}
	return file, nil
}

// Content opens a file's bytes. The caller relays the response — status,
// headers and body — unchanged, and closes it. rangeHeader is the client's
// Range header, passed on so a large file can be read in parts.
func (c *Client) Content(fileID string, inline bool, rangeHeader string) (*http.Response, error) {
	address := c.BaseURL + "/files/" + url.PathEscape(fileID) + "/content"
	if inline {
		address += "?inline=true"
	}
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	return c.do(request)
}
