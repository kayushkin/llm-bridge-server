package takeout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	activitystore "github.com/kayushkin/activity-store"
	"golang.org/x/net/html"
)

// ChromeProduct is what Chrome's own records are filed under, as My Activity
// names the product.
const ChromeProduct = "Chrome"

func urlHash(link string) string {
	digest := sha256.Sum256([]byte(link))
	return hex.EncodeToString(digest[:])[:16]
}

// chromeHistory is the part of Chrome/History.json read here. "Session" (open
// tabs), "Typed Url" and "Shared Tab Group" are not visits.
type chromeHistory struct {
	BrowserHistory *[]struct {
		Title                   string `json:"title"`
		URL                     string `json:"url"`
		TimeUsec                int64  `json:"time_usec"`
		ClientID                string `json:"client_id"`
		PageTransitionQualifier string `json:"page_transition_qualifier"`
	} `json:"Browser History"`
}

// ReadChromeHistory reads every visit in Chrome/History.json. Its times are
// microseconds since 1970, UTC; an activity keeps the second, which is the
// precision My Activity has, so the same visit in both is found by url and
// time.
func ReadChromeHistory(file io.Reader) ([]Record, error) {
	var history chromeHistory
	if err := json.NewDecoder(file).Decode(&history); err != nil {
		return nil, fmt.Errorf("not Chrome's History.json: %w", err)
	}
	if history.BrowserHistory == nil {
		return nil, fmt.Errorf(`not Chrome's History.json: there is no "Browser History"`)
	}
	records := make([]Record, 0, len(*history.BrowserHistory))
	for index, visit := range *history.BrowserHistory {
		if visit.TimeUsec <= 0 || visit.URL == "" {
			return nil, fmt.Errorf("visit %d has no time or no url", index)
		}
		records = append(records, Record{Activity: activitystore.ImportActivity{
			Product: ChromeProduct, Action: activitystore.ActionVisited, Title: visit.Title, URL: visit.URL,
			OccurredAt: visit.TimeUsec / 1_000_000, Source: activitystore.SourceChromeHistory,
			SourceRef: strconv.FormatInt(visit.TimeUsec, 10) + "-" + urlHash(visit.URL),
			Details:   activitystore.Details{ChromeClientID: visit.ClientID, PageTransition: visit.PageTransitionQualifier},
		}})
	}
	return records, nil
}

// BookmarksRead is what ReadChromeBookmarks found.
type BookmarksRead struct {
	Records []Record
	// Undated is bookmarks with no ADD_DATE, which are not an activity at a
	// time and are left out.
	Undated int
}

// ReadChromeBookmarks reads Chrome/Bookmarks.html (the Netscape bookmark
// format): each bookmark with an ADD_DATE is an activity "bookmarked" at that
// second, with its folder path.
func ReadChromeBookmarks(file io.Reader) (BookmarksRead, error) {
	tokenizer := html.NewTokenizer(file)
	var read BookmarksRead
	var folders []string
	pendingFolder := ""
	var inHeading, inLink bool
	var heading, title strings.Builder
	var href, added string
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return read, nil
			}
			return BookmarksRead{}, tokenizer.Err()
		case html.StartTagToken:
			token := tokenizer.Token()
			switch token.Data {
			case "h3":
				inHeading = true
				heading.Reset()
			case "dl":
				folders = append(folders, pendingFolder)
				pendingFolder = ""
			case "a":
				inLink = true
				title.Reset()
				href, added = attribute(token, "href"), attribute(token, "add_date")
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			switch token.Data {
			case "h3":
				inHeading = false
				pendingFolder = strings.TrimSpace(heading.String())
			case "dl":
				if len(folders) > 0 {
					folders = folders[:len(folders)-1]
				}
			case "a":
				inLink = false
				seconds, err := strconv.ParseInt(added, 10, 64)
				if err != nil && added != "" {
					return BookmarksRead{}, fmt.Errorf("bookmark %q has ADD_DATE %q, not seconds", href, added)
				}
				if seconds <= 0 {
					read.Undated++
					continue
				}
				var path []string
				for _, folder := range folders {
					if folder != "" {
						path = append(path, folder)
					}
				}
				read.Records = append(read.Records, Record{Activity: activitystore.ImportActivity{
					Product: ChromeProduct, Action: activitystore.ActionBookmarked, Title: strings.TrimSpace(title.String()), URL: href,
					OccurredAt: seconds, Source: activitystore.SourceChromeBookmarks,
					SourceRef: added + "-" + urlHash(href),
					Details:   activitystore.Details{BookmarkFolder: strings.Join(path, " / ")},
				}})
			}
		case html.TextToken:
			if inHeading {
				heading.Write(tokenizer.Text())
			}
			if inLink {
				title.Write(tokenizer.Text())
			}
		}
	}
}
