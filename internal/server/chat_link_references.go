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
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	mu        sync.Mutex
	pattern   *regexp.Regexp
	fetchedAt time.Time
}

func (p *chatLinkReferencePatterns) get(ctx context.Context, client *http.Client, referenceStoreURL string, now time.Time) (*regexp.Regexp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pattern != nil && now.Sub(p.fetchedAt) < chatLinkReferencePatternsLifetime {
		return p.pattern, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(referenceStoreURL, "/")+"/types", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reference-store's GET /types: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reference-store's GET /types answered %d", response.StatusCode)
	}
	var answer struct {
		Types []struct {
			Get        string   `json:"get"`
			IDPatterns []string `json:"id_patterns"`
		} `json:"types"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("reference-store's GET /types: %w", err)
	}
	pattern, err := joinReferencePatterns(answer.Types)
	if err != nil {
		return nil, err
	}
	p.pattern, p.fetchedAt = pattern, now
	return pattern, nil
}

// joinReferencePatterns joins the patterns of every type that can be looked
// up into one case-insensitive expression matching a whole word. A pattern
// that does not compile is left out and logged, rather than costing the rest.
func joinReferencePatterns(types []struct {
	Get        string   `json:"get"`
	IDPatterns []string `json:"id_patterns"`
}) (*regexp.Regexp, error) {
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
	Label   string `json:"label"`
	PageURL string `json:"page_url"`
	Refused bool   `json:"refused"`
}

// chatLinkReplyForRecipient answers the reply with its references drawn for
// the person the link talks with. A failure anywhere is logged and the reply
// goes as it was written: a missing link is not a wrong one.
func (s *Server) chatLinkReplyForRecipient(ctx context.Context, link store.ChatLink, body string) string {
	if s.cfg.ReferenceStoreURL == "" || s.chatLinks == nil {
		return body
	}
	session, err := s.store.GetSession(link.SessionID)
	if err != nil || session == nil {
		log.Printf("[chat-links] ERROR: %s: read session %s to draw references: %v", link.ID, link.SessionID, err)
		return body
	}
	recipient, err := s.store.ChatLinkRecipientPrincipalID(link.ID, session.PrincipalID)
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: who the reply goes to: %v", link.ID, err)
		return body
	}
	if recipient == "" {
		return body
	}
	pattern, err := s.chatLinks.references.get(ctx, s.chatLinks.httpClient, s.cfg.ReferenceStoreURL, s.chatLinks.now())
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: %v; the reply goes with its ids as written", link.ID, err)
		return body
	}
	drawn, err := drawReferencesForRecipient(ctx, body, pattern, s.settings.String(config.SettingChatLinksReferencePageBaseURL),
		func(ctx context.Context, ids []string) (map[string][]referenceMatch, error) {
			return resolveReferencesAs(ctx, s.chatLinks.httpClient, s.cfg.ReferenceStoreURL, recipient, ids)
		})
	if err != nil {
		log.Printf("[chat-links] ERROR: %s: look the reply's ids up for %s: %v; the reply goes with its ids as written", link.ID, recipient, err)
		return body
	}
	return drawn
}

// drawReferencesForRecipient replaces each id outside a protected span that
// names exactly one record the recipient may see, and that has a page to link
// to, by `[name](page)`.
func drawReferencesForRecipient(ctx context.Context, body string, pattern *regexp.Regexp, pageBaseURL string,
	resolve func(context.Context, []string) (map[string][]referenceMatch, error)) (string, error) {
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
		return body, nil
	}
	if len(ids) > 128 {
		ids = ids[:128]
	}
	matches, err := resolve(ctx, ids)
	if err != nil {
		return body, err
	}
	links := map[string]string{}
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
	}
	if len(links) == 0 {
		return body, nil
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
	return out.String(), nil
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
