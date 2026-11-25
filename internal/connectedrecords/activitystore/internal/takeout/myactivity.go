package takeout

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	activitystore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/activitystore"
	"golang.org/x/net/html"
)

// Record is one activity as read, with the names of the files that came with
// it, relative to the page they were read from.
type Record struct {
	Activity        activitystore.ImportActivity
	AttachmentFiles []string
}

// line is one line of a card: its text, the links in it, and the text before
// the first link.
type line struct {
	text            strings.Builder
	beforeFirstLink strings.Builder
	links           []activitystore.Link
	sawPlainText    bool // text outside <b>
}

func (l *line) empty() bool { return strings.TrimSpace(l.text.String()) == "" && len(l.links) == 0 }

// card is one outer-cell of a My Activity page, cut into its regions' lines.
type card struct {
	header, body, caption []*line
	files                 []string
}

type region int

const (
	regionNone region = iota
	regionHeader
	regionBody
	regionRight
	regionCaption
)

// blockTags end a line, as a <br> does: a long answer in a card is
// paragraphs and lists.
var blockTags = map[string]bool{"p": true, "li": true, "ul": true, "ol": true, "tr": true, "table": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "blockquote": true, "pre": true}

func attribute(token html.Token, name string) string {
	for _, attr := range token.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

// readCards cuts a My Activity page into cards.
func readCards(page io.Reader) ([]card, error) {
	tokenizer := html.NewTokenizer(page)
	var cards []card
	var current *card
	depth, cardDepth, regionDepth := 0, 0, 0
	where := regionNone
	var open *line
	inBold := false
	var linkHref string
	var linkText *strings.Builder
	endLine := func() {
		if current == nil || open == nil {
			return
		}
		if !open.empty() {
			switch where {
			case regionHeader:
				current.header = append(current.header, open)
			case regionBody:
				current.body = append(current.body, open)
			case regionCaption:
				current.caption = append(current.caption, open)
			}
		}
		open = &line{}
	}
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				if current != nil {
					return nil, fmt.Errorf("the page ends inside card %d", len(cards)+1)
				}
				return cards, nil
			}
			return nil, tokenizer.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			switch token.Data {
			case "div":
				if token.Type == html.SelfClosingTagToken {
					continue
				}
				depth++
				class := attribute(token, "class")
				switch {
				case strings.Contains(class, "outer-cell"):
					if current != nil {
						return nil, fmt.Errorf("card %d opens inside another", len(cards)+1)
					}
					current, cardDepth = &card{}, depth
				case current != nil && strings.Contains(class, "header-cell"):
					where, regionDepth, open = regionHeader, depth, &line{}
				case current != nil && strings.Contains(class, "content-cell"):
					regionDepth, open = depth, &line{}
					switch {
					case strings.Contains(class, "text-right"):
						where = regionRight
					case strings.Contains(class, "caption"):
						where = regionCaption
					default:
						where = regionBody
					}
				}
			case "br":
				endLine()
			case "a":
				if open != nil && where != regionNone {
					linkHref, linkText = attribute(token, "href"), &strings.Builder{}
				}
			case "b":
				inBold = true
			case "source", "img":
				if current != nil && where == regionRight {
					if source := attribute(token, "src"); source != "" {
						current.files = append(current.files, source)
					}
				}
			default:
				if blockTags[token.Data] && open != nil && !open.empty() {
					endLine()
				}
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			switch token.Data {
			case "div":
				if current != nil && depth == regionDepth && where != regionNone {
					endLine()
					where, regionDepth, open = regionNone, 0, nil
				}
				if current != nil && depth == cardDepth {
					cards = append(cards, *current)
					current, cardDepth = nil, 0
				}
				depth--
			case "a":
				if linkText != nil && open != nil {
					open.links = append(open.links, activitystore.Link{Name: strings.TrimSpace(linkText.String()), URL: linkHref})
				}
				linkText = nil
			case "b":
				inBold = false
			default:
				if blockTags[token.Data] && open != nil && !open.empty() {
					endLine()
				}
			}
		case html.TextToken:
			if open == nil || where == regionNone || where == regionRight {
				continue
			}
			text := string(tokenizer.Text())
			open.text.WriteString(text)
			if len(open.links) == 0 && linkText == nil {
				open.beforeFirstLink.WriteString(text)
			}
			if linkText != nil {
				linkText.WriteString(text)
			}
			if !inBold && strings.TrimSpace(text) != "" {
				open.sawPlainText = true
			}
		}
	}
}

// cleanText trims spaces, em spaces and no-break spaces from both ends.
func cleanText(text string) string {
	return strings.Trim(text, " \t\r\n   ")
}

// coordinatesOf reads latitude and longitude from a Google Maps link's center
// or query parameter.
func coordinatesOf(link string) (*float64, *float64) {
	parsed, err := url.Parse(link)
	if err != nil {
		return nil, nil
	}
	for _, name := range []string{"center", "query"} {
		value := parsed.Query().Get(name)
		latitudeText, longitudeText, found := strings.Cut(value, ",")
		if !found {
			continue
		}
		latitude, errLatitude := strconv.ParseFloat(strings.TrimSpace(latitudeText), 64)
		longitude, errLongitude := strconv.ParseFloat(strings.TrimSpace(longitudeText), 64)
		if errLatitude == nil && errLongitude == nil && latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180 {
			return &latitude, &longitude
		}
	}
	return nil, nil
}

// locationOf reads one "Locations" line: "At <place> - From your device".
func locationOf(value *line) activitystore.Location {
	text := cleanText(value.text.String())
	location := activitystore.Location{Basis: text}
	for _, link := range value.links {
		latitude, longitude := coordinatesOf(link.URL)
		if latitude == nil {
			continue
		}
		location.Name, location.URL, location.Latitude, location.Longitude = link.Name, link.URL, latitude, longitude
		break
	}
	if _, basis, found := strings.Cut(text, " - "); found {
		location.Basis = cleanText(basis)
	}
	return location
}

// isChannelLine says whether a line is only a link to a YouTube channel.
func isChannelLine(value *line) bool {
	if len(value.links) != 1 || cleanText(value.text.String()) != value.links[0].Name {
		return false
	}
	link := value.links[0].URL
	return strings.Contains(link, "youtube.com/channel/") || strings.Contains(link, "youtube.com/@") || strings.Contains(link, "youtube.com/user/")
}

// MyActivityPage is one page to read and what to call its records.
type MyActivityPage struct {
	// Product is what every record of the page is filed under: the page's
	// directory name in My Activity, or YouTube for YouTube's own history.
	Product string
	Source  string // activitystore.SourceGoogleMyActivity or SourceYouTubeHistory
}

// ReadMyActivity reads every card of a My Activity page (or YouTube's watch
// or search history, which is the same HTML). A card without a time it can
// read is an error naming the card: nothing is guessed.
func ReadMyActivity(page io.Reader, about MyActivityPage) ([]Record, error) {
	cards, err := readCards(page)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(cards))
	seen := map[string]int{}
	for index, read := range cards {
		record, err := recordOf(read, about)
		if err != nil {
			return nil, fmt.Errorf("card %d: %w", index+1, err)
		}
		// The same words at the same second twice in one page are two records;
		// counting them keeps both and keeps each one's reference the same
		// from export to export.
		base := record.Activity.SourceRef
		seen[base]++
		if seen[base] > 1 {
			record.Activity.SourceRef = fmt.Sprintf("%s#%d", base, seen[base])
		}
		records = append(records, record)
	}
	return records, nil
}

func recordOf(read card, about MyActivityPage) (Record, error) {
	if len(read.body) == 0 {
		return Record{}, fmt.Errorf("the card has no body")
	}
	timeLine := -1
	for index := 1; index < len(read.body); index++ {
		if IsMyActivityTime(read.body[index].text.String()) {
			timeLine = index
			break
		}
	}
	if timeLine < 0 {
		return Record{}, fmt.Errorf("the card has no time line")
	}
	occurredAt, err := ParseMyActivityTime(read.body[timeLine].text.String())
	if err != nil {
		return Record{}, err
	}
	activity := activitystore.ImportActivity{Product: about.Product, Source: about.Source, OccurredAt: occurredAt}

	first := read.body[0]
	if len(first.links) > 0 {
		phrase := cleanText(first.beforeFirstLink.String())
		activity.Title, activity.URL = first.links[0].Name, first.links[0].URL
		if phrase == "" {
			activity.Action = activitystore.ActionViewed
		} else {
			activity.Action, _ = activitystore.ActionForPhrase(phrase)
			activity.ActionText = phrase
		}
	} else {
		text := cleanText(first.text.String())
		if phrase, rest, found := strings.Cut(text, " "); found {
			activity.Action, _ = activitystore.ActionForPhrase(cleanText(phrase))
			activity.ActionText, activity.Title = cleanText(phrase), cleanText(rest)
		} else {
			activity.Action, activity.Title = activitystore.ActionOther, text
		}
	}

	var headerParts []string
	for _, part := range read.header {
		headerParts = append(headerParts, cleanText(part.text.String()))
	}
	if header := strings.Join(headerParts, " "); header != about.Product {
		activity.Details.Header = header
	}
	for index, value := range read.body {
		if index == 0 || index == timeLine {
			continue
		}
		if activity.Details.Channel == nil && index == 1 && isChannelLine(value) {
			activity.Details.Channel = &activitystore.Link{Name: value.links[0].Name, URL: value.links[0].URL}
			continue
		}
		extra := activitystore.Line{Text: cleanText(value.text.String())}
		if len(value.links) > 0 {
			extra.URL = value.links[0].URL
		}
		activity.Details.Lines = append(activity.Details.Lines, extra)
	}
	label := ""
	for _, value := range read.caption {
		text := cleanText(value.text.String())
		if !value.sawPlainText && (strings.HasSuffix(text, ":") || strings.HasSuffix(text, "?")) {
			label = text
			continue
		}
		switch label {
		case "Products:":
			activity.Details.Products = append(activity.Details.Products, text)
		case "Details:":
			activity.Details.Notes = append(activity.Details.Notes, text)
		case "Locations:":
			location := locationOf(value)
			activity.Details.Locations = append(activity.Details.Locations, location)
			if activity.Latitude == nil && location.Latitude != nil {
				activity.Latitude, activity.Longitude = location.Latitude, location.Longitude
			}
		}
	}
	activity.SourceRef = myActivityReference(about.Product, first, occurredAt)
	return Record{Activity: activity, AttachmentFiles: read.files}, nil
}

// myActivityReference is what finds a card again in a later export: its
// product, its first line's words and link, and its time. It is a hash, so a
// purged activity keeps no words in its references.
func myActivityReference(product string, first *line, occurredAt int64) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%s\x00%d", product, cleanText(first.text.String()), occurredAt)
	for _, link := range first.links {
		fmt.Fprintf(digest, "\x00%s", link.URL)
	}
	return hex.EncodeToString(digest.Sum(nil))[:32]
}
