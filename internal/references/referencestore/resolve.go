package referencestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits on one POST /resolve.
const (
	MaxIDsPerAsk  = 128
	MaxIDLength   = 256
	maxRecordSize = 1 << 20
	// maxCachedRecords is when a write first sweeps out expired answers.
	maxCachedRecords = 20000
)

// Match is one record an id names.
//
// Data is the owning store's answer, unchanged. Label and PageURL are read
// from it by the type's row; a refused match carries neither, nor Data.
type Match struct {
	Type    string          `json:"type"`
	Service string          `json:"service"`
	Label   string          `json:"label,omitempty"`
	PageURL string          `json:"page_url,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	// Refused says the record exists and the caller may not see it.
	Refused bool `json:"refused,omitempty"`
}

// LookupError is a store that could not say whether it holds an id. It is
// never cached, so the next ask tries again.
type LookupError struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Error string `json:"error"`
}

// Resolver looks ids up in the stores that own them.
type Resolver struct {
	Types         []ReferenceType
	ServiceURLs   map[string]string
	SiteURLs      map[string]string
	Credentials   *Credentials
	Access        *AccessChecker
	HTTPClient    *http.Client
	RecordTTL     time.Duration
	MissTTL       time.Duration
	LookupTimeout time.Duration
	Concurrency   int
	now           func() time.Time

	mu    sync.Mutex
	cache map[string]cachedRecord
}

// cachedRecord is a store's answer for (type, id), or its 404. Records of an
// owner-decided type are cached per caller, because the owner's answer
// depends on who asks.
type cachedRecord struct {
	data      json.RawMessage
	found     bool
	refused   bool
	expiresAt time.Time
}

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Caller is who an ask is for. Unchecked callers act for nobody and see
// everything: only an internal service presenting the service token is one.
type Caller struct {
	PrincipalID string
	Unchecked   bool
}

// Resolve answers every id: the records it names that the caller may see,
// those it may not marked refused, and an empty list for an id nothing holds.
func (r *Resolver) Resolve(ctx context.Context, caller Caller, ids []string) (map[string][]Match, []LookupError) {
	results := make(map[string][]Match, len(ids))
	type probe struct {
		id  string
		row *ReferenceType
	}
	var probes []probe
	for _, id := range ids {
		results[id] = []Match{}
		for i := range r.Types {
			row := &r.Types[i]
			if row.Resolvable() && row.Matches(id) {
				probes = append(probes, probe{id, row})
			}
		}
	}
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		errs      []LookupError
		semaphore = make(chan struct{}, max(1, r.Concurrency))
	)
	for _, p := range probes {
		wg.Add(1)
		go func(p probe) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			match, found, err := r.lookUp(ctx, caller, p.row, p.id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, LookupError{ID: p.id, Type: p.row.Type, Error: err.Error()})
				log.Printf("[reference-store] %s %s: %v", p.row.Type, p.id, err)
				return
			}
			if found {
				results[p.id] = append(results[p.id], match)
			}
		}(p)
	}
	wg.Wait()
	return results, errs
}

// lookUp fetches one (type, id) and applies the caller's access to it.
func (r *Resolver) lookUp(ctx context.Context, caller Caller, row *ReferenceType, id string) (Match, bool, error) {
	ownerDecides := row.Access == AccessByOwner && !caller.Unchecked
	cacheKey := row.Type + "\x00" + id
	if ownerDecides {
		cacheKey += "\x00" + caller.PrincipalID
	}
	record, err := r.cachedFetch(ctx, cacheKey, row, id, caller, ownerDecides)
	if err != nil {
		return Match{}, false, err
	}
	if !record.found {
		return Match{}, false, nil
	}
	data := record.data
	match := Match{Type: row.Type, Service: row.Service}
	if record.refused {
		match.Refused = true
		return match, true, nil
	}
	if !caller.Unchecked && row.Access == AccessByGrant {
		allowed, err := r.Access.MaySee(ctx, caller.PrincipalID, row.Type, id)
		if err != nil {
			return Match{}, false, err
		}
		if !allowed {
			match.Refused = true
			return match, true, nil
		}
	}
	match.Data = data
	match.Label = labelOf(row, data)
	match.PageURL = r.pageURL(row, id, data)
	return match, true, nil
}

// errOwnerRefused is the owning service, asked as the caller, answering 403.
var errOwnerRefused = errors.New("refused by owner")

func (r *Resolver) cachedFetch(ctx context.Context, key string, row *ReferenceType, id string, caller Caller, asCaller bool) (cachedRecord, error) {
	now := r.clock()
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedRecord{}
	}
	if cached, ok := r.cache[key]; ok && now.Before(cached.expiresAt) {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	data, found, err := r.fetch(ctx, row, id, caller, asCaller)
	record := cachedRecord{data: data, found: found}
	if errors.Is(err, errOwnerRefused) {
		record, err = cachedRecord{found: true, refused: true}, nil
	}
	if err != nil {
		return cachedRecord{}, err
	}
	ttl := r.RecordTTL
	if !record.found {
		ttl = r.MissTTL
	}
	record.expiresAt = now.Add(ttl)
	r.mu.Lock()
	if len(r.cache) >= maxCachedRecords {
		for cachedKey, cached := range r.cache {
			if !now.Before(cached.expiresAt) {
				delete(r.cache, cachedKey)
			}
		}
	}
	r.cache[key] = record
	r.mu.Unlock()
	return record, nil
}

func (r *Resolver) fetch(ctx context.Context, row *ReferenceType, id string, caller Caller, asCaller bool) (json.RawMessage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.LookupTimeout)
	defer cancel()
	baseURL, err := r.serviceURL(row.Service)
	if err != nil {
		return nil, false, err
	}
	target := baseURL + strings.Replace(row.Get, "{id}", url.PathEscape(id), 1)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, err
	}
	if err := r.Credentials.Apply(ctx, request, row); err != nil {
		return nil, false, err
	}
	if asCaller {
		request.Header.Set(PrincipalIDHeader, caller.PrincipalID)
	}
	response, err := r.HTTPClient.Do(request)
	if err != nil {
		return nil, false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRecordSize+1))
	if err != nil {
		return nil, false, err
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, false, nil
	case asCaller && response.StatusCode == http.StatusForbidden:
		return nil, false, errOwnerRefused
	case response.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("%s answered %d: %s", row.Service, response.StatusCode, firstLine(body))
	case len(body) > maxRecordSize:
		return nil, false, fmt.Errorf("%s answered more than %d bytes", row.Service, maxRecordSize)
	case !json.Valid(body):
		return nil, false, fmt.Errorf("%s answered something that is not JSON", row.Service)
	}
	return body, true, nil
}

func firstLine(body []byte) string {
	text := strings.TrimSpace(string(body))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// fieldAt reads a dotted path (messages.0.room_id) from a record. Only a
// string, number or boolean is a value; anything else is absent.
func fieldAt(data json.RawMessage, path string) (string, bool) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return "", false
	}
	for _, step := range strings.Split(path, ".") {
		switch node := value.(type) {
		case map[string]any:
			value = node[step]
		case []any:
			index, err := strconv.Atoi(step)
			if err != nil || index < 0 || index >= len(node) {
				return "", false
			}
			value = node[index]
		default:
			return "", false
		}
	}
	switch v := value.(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

func labelOf(row *ReferenceType, data json.RawMessage) string {
	if row.LabelField == "" {
		return ""
	}
	label, _ := fieldAt(data, row.LabelField)
	return label
}

// pageURL fills the row's page template. A placeholder whose field the record
// does not have leaves the record with no page link rather than a wrong one.
func (r *Resolver) pageURL(row *ReferenceType, id string, data json.RawMessage) string {
	if row.Page == "" {
		return ""
	}
	var builder strings.Builder
	rest := row.Page
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			builder.WriteString(rest)
			break
		}
		closing := strings.IndexByte(rest[open:], '}')
		if closing < 0 {
			log.Printf("[reference-store] type %s: page %q has an unclosed placeholder", row.Type, row.Page)
			return ""
		}
		builder.WriteString(rest[:open])
		placeholder := rest[open+1 : open+closing]
		rest = rest[open+closing+1:]
		var value string
		if placeholder == "id" {
			value = id
		} else if path, ok := strings.CutPrefix(placeholder, "data."); ok {
			found := false
			value, found = fieldAt(data, path)
			if !found || value == "" {
				return ""
			}
		} else {
			log.Printf("[reference-store] type %s: page %q has an unknown placeholder {%s}", row.Type, row.Page, placeholder)
			return ""
		}
		builder.WriteString(url.QueryEscape(value))
	}
	page := builder.String()
	if row.PageSite != "" {
		siteURL := r.SiteURLs[row.PageSite]
		if siteURL == "" {
			return ""
		}
		return strings.TrimRight(siteURL, "/") + page
	}
	return page
}

// MaximumAttachmentBytes is the largest picture an attachment carries: what a
// chat platform takes as one file (Discord's limit for a bot).
const MaximumAttachmentBytes = 10 << 20

// ErrNoAttachment is an id that names no one record the caller may see with a
// picture: unknown, refused, claimed by several stores, or of a type with none.
var ErrNoAttachment = errors.New("no attachment")

// Attachment is a picture of the one record id names, for the caller: its
// bytes and content type, from the owning service's attachment route.
func (r *Resolver) Attachment(ctx context.Context, caller Caller, id string) (string, []byte, error) {
	results, errs := r.Resolve(ctx, caller, []string{id})
	if len(errs) > 0 {
		return "", nil, fmt.Errorf("%s: %s", errs[0].Type, errs[0].Error)
	}
	matches := results[id]
	if len(matches) != 1 || matches[0].Refused {
		return "", nil, ErrNoAttachment
	}
	var row *ReferenceType
	for i := range r.Types {
		if r.Types[i].Type == matches[0].Type {
			row = &r.Types[i]
		}
	}
	if row == nil || row.Attachment == "" {
		return "", nil, ErrNoAttachment
	}
	ctx, cancel := context.WithTimeout(ctx, r.LookupTimeout)
	defer cancel()
	baseURL, err := r.serviceURL(row.Service)
	if err != nil {
		return "", nil, err
	}
	target := baseURL + strings.Replace(row.Attachment, "{id}", url.PathEscape(id), 1)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", nil, err
	}
	if err := r.Credentials.Apply(ctx, request, row); err != nil {
		return "", nil, err
	}
	response, err := r.HTTPClient.Do(request)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// The record has no picture (an outfit nobody photographed).
		return "", nil, ErrNoAttachment
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaximumAttachmentBytes+1))
	if err != nil {
		return "", nil, err
	}
	if response.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%s answered %d: %s", row.Service, response.StatusCode, firstLine(body))
	}
	if len(body) > MaximumAttachmentBytes {
		return "", nil, fmt.Errorf("%s's picture of %s is over %d bytes", row.Service, id, MaximumAttachmentBytes)
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		return "", nil, fmt.Errorf("%s answered %q for a picture of %s", row.Service, contentType, id)
	}
	return contentType, body, nil
}

// serviceURL refuses to turn an unconfigured service into a relative request.
func (r *Resolver) serviceURL(service string) (string, error) {
	address := r.ServiceURLs[service]
	if address == "" {
		return "", fmt.Errorf("service %q is not configured", service)
	}
	return strings.TrimRight(address, "/"), nil
}
