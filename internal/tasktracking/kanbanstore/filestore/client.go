// Package filestore is kanban-store's client for file-store, which owns
// uploaded bytes. kanban-store owns which files hang on which card and who may
// see them; file-store owns the file. So this client carries bytes and
// file-store's own answers through unchanged, and re-describes neither.
package filestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OwnerService is how kanban-store names itself as the owner of a file.
const OwnerService = "kanban-store"

// ServiceTokenHeader is what file-store asks of every caller.
const ServiceTokenHeader = "X-File-Store-Service-Token"

// File is file-store's record, passed through as it was sent. Only the fields
// kanban-store itself reads are named; the rest ride along in Raw.
type File struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
	// Raw is the record exactly as file-store wrote it, which is what goes out
	// on kanban-store's wire.
	Raw json.RawMessage `json:"-"`
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

func decodeFile(body []byte) (*File, error) {
	var file File
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("file-store answered a file that does not parse: %w", err)
	}
	file.Raw = append(json.RawMessage(nil), body...)
	return &file, nil
}

// Upload streams content to file-store as a file of cardID's.
func (c *Client) Upload(content io.Reader, contentLength int64, contentType, filename, cardID, uploadedByPrincipalID string) (*File, error) {
	query := url.Values{"filename": {filename}, "owner_service": {OwnerService}, "owner_ref": {cardID}}
	if uploadedByPrincipalID != "" {
		query.Set("uploaded_by_principal_id", uploadedByPrincipalID)
	}
	request, err := http.NewRequest(http.MethodPost, c.BaseURL+"/files?"+query.Encode(), content)
	if err != nil {
		return nil, err
	}
	request.ContentLength = contentLength
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusCreated {
		return nil, &RefusalError{Status: response.StatusCode, Body: body}
	}
	return decodeFile(body)
}

// FilesOfCard lists every live file kanban-store holds for a card, by id.
func (c *Client) FilesOfCard(cardID string) (map[string]*File, error) {
	return c.filesOfCard(cardID, false)
}

// EveryFileOfCard is FilesOfCard with the files file-store holds as deleted: the
// ones that were taken off the card, which file-store keeps so that a removal
// can be undone. Purging a card has to reach those too.
func (c *Client) EveryFileOfCard(cardID string) (map[string]*File, error) {
	return c.filesOfCard(cardID, true)
}

func (c *Client) filesOfCard(cardID string, includeDeleted bool) (map[string]*File, error) {
	files := map[string]*File{}
	const page = 500
	for offset := 0; ; offset += page {
		query := url.Values{"owner_service": {OwnerService}, "owner_ref": {cardID}, "limit": {fmt.Sprint(page)}, "offset": {fmt.Sprint(offset)}}
		if includeDeleted {
			query.Set("include_deleted", "true")
		}
		request, err := http.NewRequest(http.MethodGet, c.BaseURL+"/files?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		response, err := c.do(request)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, &RefusalError{Status: response.StatusCode, Body: body}
		}
		var raws []json.RawMessage
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, fmt.Errorf("file-store answered a list that does not parse: %w", err)
		}
		for _, raw := range raws {
			file, err := decodeFile(raw)
			if err != nil {
				return nil, err
			}
			files[file.ID] = file
		}
		if len(raws) < page {
			return files, nil
		}
	}
}

// Content opens a file's bytes. The response is file-store's own — status,
// headers and body — for the caller to pass on and close. forwarded carries
// the reader's Range and If-None-Match.
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

// Delete removes a file. Reversible unless purge, which destroys it.
func (c *Client) Delete(fileID string, purge bool) error {
	target := c.BaseURL + "/files/" + url.PathEscape(fileID)
	if purge {
		target += "?hard=true"
	}
	request, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	response, err := c.do(request)
	if err != nil {
		return err
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
