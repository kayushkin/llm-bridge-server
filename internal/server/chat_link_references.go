package server

// A chat link's reply, as the person it goes to may read it.
//
// A session's reply names records by id (prediction_000012, a noteboard uuid,
// repo:dash). In the chat those draw as reference chips; a person on Discord
// sees only text. So before a reply is sent, each id in it that names a record
// the person may see becomes the record's name linked to its page, and every
// other id stays as written. "May see" is the person's own access, never the
// session's: the ids are looked up in reference-store as the principal
// people-store ties the person's account to, so a private record the operator
// can see reaches nobody else. A person with no principal sees every id as it
// was written. A link in a code span, a URL or an existing markdown link is
// left alone, as is an id several stores claim.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// chatLinkReferencePatternsLifetime is how long reference-store's id patterns
// are reused before they are read again.
const chatLinkReferencePatternsLifetime = 5 * time.Minute

// chatLinkReferencePatterns is reference-store's id patterns, joined into one
// expression, read at most every chatLinkReferencePatternsLifetime.
type chatLinkReferencePatterns struct {
	mu      sync.Mutex
	pattern *regexp.Regexp
	// withPictures are the types whose rows name an attachment route.
	withPictures map[string]bool
	fetchedAt    time.Time
}

func (p *chatLinkReferencePatterns) get(ctx context.Context, client *http.Client, referenceStoreURL string, now time.Time) (*regexp.Regexp, map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pattern != nil && now.Sub(p.fetchedAt) < chatLinkReferencePatternsLifetime {
		return p.pattern, p.withPictures, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(referenceStoreURL, "/")+"/types", nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("reference-store's GET /types: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("reference-store's GET /types answered %d", response.StatusCode)
	}
	var answer struct {
		Types []referenceTypeRow `json:"types"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return nil, nil, fmt.Errorf("reference-store's GET /types: %w", err)
	}
	pattern, err := joinReferencePatterns(answer.Types)
	if err != nil {
		return nil, nil, err
	}
	withPictures := map[string]bool{}
	for _, row := range answer.Types {
		if row.Attachment != "" {
			withPictures[row.Type] = true
		}
	}
	p.pattern, p.withPictures, p.fetchedAt = pattern, withPictures, now
	return pattern, withPictures, nil
}

// referenceTypeRow is the part of a reference-store type row a chat link reads.
type referenceTypeRow struct {
	Type       string   `json:"type"`
	Get        string   `json:"get"`
	IDPatterns []string `json:"id_patterns"`
	Attachment string   `json:"attachment"`
}

// joinReferencePatterns joins the patterns of every type that can be looked
// up into one case-insensitive expression matching a whole word. A pattern
// that does not compile is left out and logged, rather than costing the rest.
func joinReferencePatterns(types []referenceTypeRow) (*regexp.Regexp, error) {
	var usable []string
	for _, row := range types {
		if row.Get == "" {
			continue
		}
		for _, pattern := range row.IDPatterns {
			if _, err := regexp.Compile(pattern); err != nil {
				log.Printf("[chat-links] ERROR: reference-store's id pattern %q does not compile: %v", pattern, err)
				continue
			}
			usable = append(usable, "(?:"+pattern+")")
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("reference-store lists no id pattern that can be looked up")
	}
	return regexp.Compile(`(?i)\b(?:` + strings.Join(usable, "|") + `)\b`)
}

// protectedSpansOfReply are the parts of a reply no id is drawn in: code
// spans, markdown links and bare URLs.
var protectedSpansOfReply = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`|\\[[^\\]\n]*\\]\\([^)\n]*\\)|https?://\\S+")

// referenceMatch is one record reference-store found for an id, as much of
// it as drawing a link needs.
type referenceMatch struct {
	Type    string `json:"type"`
	Label   string `json:"label"`
	PageURL string `json:"page_url"`
	Refused bool   `json:"refused"`
}

// chatLinkFile is a file sent with a reply, as multichat's bot send takes it.
type chatLinkFile struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	DataBase64  string `json:"data_base64"`
}

// chatLinkMaximumAttachments is how many pictures one reply carries: what a
// Discord message takes.
const chatLinkMaximumAttachments = 10

// chatLinkReplyForRecipient answers the reply with its references drawn for
// the person the link talks with, and a picture of each linked record that
// has one. A failure anywhere is logged and the reply goes as it was written,
// or without a picture: a missing link is not a wrong one.
func (s *Server) chatLinkReplyForRecipient(ctx context.Context, link store.ChatLink, body string) (string, []chatLinkFile) {
	if s.cfg.ReferenceStoreURL == "" || s.chatLinks == nil {
		return body, nil
	}
	session, err := s.store.GetSession(link.SessionID)
	if err != nil || session == nil {
		log.Printf("[chat-links] ERROR: %s: read session %s to draw references: %v", link.ID, link.SessionID, err)
		return body, nil
	}
	recipient, err := s.store.ChatLinkRecipientPrincipalID(link.ID, session.PrincipalID)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: who the reply goes to: %v", link.ID, err)
		return body, nil
	}
	if recipient == "" {
		return body, nil
	}
	pattern, withPictures, err := s.chatLinks.references.get(ctx, s.chatLinks.httpClient, s.cfg.ReferenceStoreURL, s.chatLinks.now())
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: %v; the reply goes with its ids as written", link.ID, err)
		return body, nil
	}
	drawn, linked, err := drawReferencesForRecipient(ctx, body, pattern, s.settings.String(config.SettingChatLinksReferencePageBaseURL),
		func(ctx context.Context, ids []string) (map[string][]referenceMatch, error) {
			return resolveReferencesAs(ctx, s.chatLinks.httpClient, s.cfg.ReferenceStoreURL, recipient, ids)
		})
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: look the reply's ids up for %s: %v; the reply goes with its ids as written", link.ID, recipient, err)
		return body, nil
	}
	var files []chatLinkFile
	for _, reference := range linked {
		if len(files) == chatLinkMaximumAttachments {
			break
		}
		if !withPictures[reference.Type] {
			continue
		}
		file, found, err := pictureOfReferenceAs(ctx, s.chatLinks.httpClient, s.cfg.ReferenceStoreURL, recipient, reference.ID)
		if err != nil {
			log.Printf("[chat-links] ERROR: %s: the picture of %s for %s: %v; sent without it", link.ID, reference.ID, recipient, err)
			continue
		}
		if found {
			files = append(files, file)
		}
	}
	return drawn, files
}

// linkedReference is a record a reply drew as a link.
type linkedReference struct {
	ID   string
	Type string
}

// pictureOfReferenceAs asks reference-store for a picture of the record, as
// principalID. A 404 is no picture, not a failure.
func pictureOfReferenceAs(ctx context.Context, client *http.Client, referenceStoreURL, principalID, id string) (chatLinkFile, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(referenceStoreURL, "/")+"/attachments/"+url.PathEscape(id), nil)
	if err != nil {
		return chatLinkFile{}, false, err
	}
	request.Header.Set(principalIdentityHeader, principalID)
	response, err := client.Do(request)
	if err != nil {
		return chatLinkFile{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return chatLinkFile{}, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 10<<20+1))
	if err != nil {
		return chatLinkFile{}, false, err
	}
	if response.StatusCode != http.StatusOK {
		return chatLinkFile{}, false, fmt.Errorf("reference-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) > 10<<20 {
		return chatLinkFile{}, false, fmt.Errorf("the picture is over 10 MiB")
	}
	contentType := response.Header.Get("Content-Type")
	extensions, _ := mime.ExtensionsByType(contentType)
	name := id
	if len(extensions) > 0 {
		name += extensions[0]
	}
	return chatLinkFile{Name: name, ContentType: contentType, DataBase64: base64.StdEncoding.EncodeToString(data)}, true, nil
}

// drawReferencesForRecipient replaces each id outside a protected span that
// names exactly one record the recipient may see, and that has a page to link
// to, by `[name](page)`.
func drawReferencesForRecipient(ctx context.Context, body string, pattern *regexp.Regexp, pageBaseURL string,
	resolve func(context.Context, []string) (map[string][]referenceMatch, error)) (string, []linkedReference, error) {
	protected := protectedSpansOfReply.FindAllStringIndex(body, -1)
	inProtected := func(start, end int) bool {
		for _, span := range protected {
			if start < span[1] && end > span[0] {
				return true
			}
		}
		return false
	}
	var spans [][]int
	seen := map[string]bool{}
	var ids []string
	for _, span := range pattern.FindAllStringIndex(body, -1) {
		if inProtected(span[0], span[1]) {
			continue
		}
		spans = append(spans, span)
		if id := body[span[0]:span[1]]; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return body, nil, nil
	}
	if len(ids) > 128 {
		ids = ids[:128]
	}
	matches, err := resolve(ctx, ids)
	if err != nil {
		return body, nil, err
	}
	links := map[string]string{}
	var linked []linkedReference
	for _, id := range ids {
		found := matches[id]
		if len(found) != 1 || found[0].Refused {
			continue
		}
		page := referencePageForChat(found[0].PageURL, id, pageBaseURL)
		if page == "" {
			continue
		}
		label := strings.TrimSpace(found[0].Label)
		if label == "" {
			label = id
		}
		links[id] = "[" + strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(label) + "](" + page + ")"
		linked = append(linked, linkedReference{ID: id, Type: found[0].Type})
	}
	if len(links) == 0 {
		return body, nil, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var out strings.Builder
	last := 0
	for _, span := range spans {
		link, ok := links[body[span[0]:span[1]]]
		if !ok {
			continue
		}
		out.WriteString(body[last:span[0]])
		out.WriteString(link)
		last = span[1]
	}
	out.WriteString(body[last:])
	return out.String(), linked, nil
}

// referencePageForChat is the address a record's link points at: its own
// page, on its own site or on pageBaseURL, else the generic record page there.
// With no pageBaseURL only a page on a site of its own can be linked.
func referencePageForChat(pageURL, id, pageBaseURL string) string {
	if strings.HasPrefix(pageURL, "https://") || strings.HasPrefix(pageURL, "http://") {
		return pageURL
	}
	base := strings.TrimRight(pageBaseURL, "/")
	if base == "" {
		return ""
	}
	if pageURL != "" {
		return base + pageURL
	}
	return base + "/ref/" + url.PathEscape(id)
}

// resolveReferencesAs asks reference-store what each id names, as principalID.
func resolveReferencesAs(ctx context.Context, client *http.Client, referenceStoreURL, principalID string, ids []string) (map[string][]referenceMatch, error) {
	payload, err := json.Marshal(map[string][]string{"ids": ids})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(referenceStoreURL, "/")+"/resolve", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(principalIdentityHeader, principalID)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reference-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(answer)))
	}
	var decoded struct {
		Results map[string][]referenceMatch `json:"results"`
	}
	if err := json.Unmarshal(answer, &decoded); err != nil {
		return nil, err
	}
	return decoded.Results, nil
}
