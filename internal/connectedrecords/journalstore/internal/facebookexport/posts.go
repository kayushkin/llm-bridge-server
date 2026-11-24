package facebookexport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kayushkin/journal-store/internal/transcripttext"
)

// PostsFile is where the export keeps the user's own posts, under
// your_facebook_activity.
const PostsFile = "posts/your_posts__check_ins__photos_and_videos_1.json"

// Post is one of the user's own posts: a status, a photo, a link or a
// check-in.
type Post struct {
	Time   time.Time
	Action string   // Facebook's line: "<name> updated their status."
	Text   string   // what the user wrote, if anything
	Lines  []string // what it carried: "photo: …", "link: …", "place: …"
	Tagged []string // people tagged, by display name
}

type postFile struct {
	Timestamp int64  `json:"timestamp"`
	Title     string `json:"title"`
	Data      []struct {
		Post string `json:"post"`
	} `json:"data"`
	Attachments []struct {
		Data []struct {
			Media *struct {
				URI         string `json:"uri"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"media"`
			ExternalContext *struct {
				URL  string `json:"url"`
				Name string `json:"name"`
			} `json:"external_context"`
			Place *struct {
				Name string `json:"name"`
			} `json:"place"`
			Event *struct {
				Name string `json:"name"`
			} `json:"event"`
		} `json:"data"`
	} `json:"attachments"`
	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
}

// ReadPosts reads the posts file.
func ReadPosts(file string) ([]Post, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var parsed []postFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	posts := make([]Post, 0, len(parsed))
	for _, entry := range parsed {
		post := Post{Time: time.Unix(entry.Timestamp, 0).UTC(), Action: Repair(entry.Title)}
		var texts []string
		for _, data := range entry.Data {
			if text := strings.TrimSpace(Repair(data.Post)); text != "" {
				texts = append(texts, text)
			}
		}
		post.Text = strings.Join(texts, "\n\n")
		for _, attachment := range entry.Attachments {
			for _, data := range attachment.Data {
				switch {
				case data.Media != nil:
					line := "photo: " + path.Base(data.Media.URI)
					if description := strings.TrimSpace(Repair(data.Media.Description)); description != "" && description != post.Text {
						line += " — " + description
					}
					post.Lines = append(post.Lines, line)
				case data.ExternalContext != nil && data.ExternalContext.URL != "":
					line := "link: " + Repair(data.ExternalContext.URL)
					if name := Repair(data.ExternalContext.Name); name != "" {
						line += " (" + name + ")"
					}
					post.Lines = append(post.Lines, line)
				case data.Place != nil:
					post.Lines = append(post.Lines, "place: "+Repair(data.Place.Name))
				case data.Event != nil:
					post.Lines = append(post.Lines, "event: "+Repair(data.Event.Name))
				}
			}
		}
		for _, tag := range entry.Tags {
			post.Tagged = append(post.Tagged, Repair(tag.Name))
		}
		posts = append(posts, post)
	}
	return posts, nil
}

// titleLength is the most runes of a post's text its entry title keeps.
const titleLength = 70

// Title is the post's first line, cut to titleLength runes, or Facebook's
// action line when the post has no text.
func (post Post) Title(selfName string) string {
	firstLine, _, _ := strings.Cut(post.Text, "\n")
	if firstLine == "" {
		return strings.TrimSuffix(strings.Replace(post.Action, selfName, "Facebook:", 1), ".")
	}
	if utf8.RuneCountInString(firstLine) > titleLength {
		runes := []rune(firstLine)
		return strings.TrimSpace(string(runes[:titleLength])) + "…"
	}
	return firstLine
}

// Body is the post as markdown: its text, what it carried, and who it
// tagged, with a chip for each tagged person people maps.
func (post Post) Body(sourceEntry string, people map[string]string) string {
	var body strings.Builder
	if post.Text != "" {
		for _, line := range strings.Split(post.Text, "\n") {
			fmt.Fprintln(&body, transcripttext.MarkdownSafe(line))
		}
		fmt.Fprintln(&body)
	}
	for _, line := range post.Lines {
		fmt.Fprintf(&body, "- _%s_\n", transcripttext.MarkdownSafe(line))
	}
	if len(post.Tagged) > 0 {
		var tagged []string
		for _, name := range post.Tagged {
			if personID := people[name]; personID != "" {
				tagged = append(tagged, fmt.Sprintf("[%s](ref:%s)", transcripttext.MarkdownSafe(name), personID))
			} else {
				tagged = append(tagged, transcripttext.MarkdownSafe(name))
			}
		}
		fmt.Fprintf(&body, "- _with_ %s\n", strings.Join(tagged, ", "))
	}
	if len(post.Lines) > 0 || len(post.Tagged) > 0 {
		fmt.Fprintln(&body)
	}
	fmt.Fprintf(&body, "%s", transcripttext.MarkdownSafe(post.Action))
	if sourceEntry != "" {
		fmt.Fprintf(&body, " From the [Facebook export](ref:%s).", sourceEntry)
	}
	fmt.Fprintln(&body)
	return body.String()
}

// SourceRef names the post for re-imports: its time, then which of the
// posts at that same second it is.
func SourceRef(post Post, sameSecond int) string {
	return fmt.Sprintf("post/%d/%d", post.Time.Unix(), sameSecond)
}

// Hash is the digest of the entry's title and body, so a re-import skips
// a post whose text did not change.
func Hash(title, body string) string {
	digest := sha256.Sum256([]byte(title + "\n" + body))
	return hex.EncodeToString(digest[:])
}
