package facebookexport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostsKeepTextAttachmentsAndTags(t *testing.T) {
	file := filepath.Join(t.TempDir(), "posts.json")
	content := `[{"timestamp":1250000000,"title":"Me Self updated their status.","data":[{"post":"cafÃ© day # one"},{"update_timestamp":1}],
	 "tags":[{"name":"ZoÃ«"}]},
	 {"timestamp":1260000000,"title":"Me Self added a new photo.","attachments":[{"data":[{"media":{"uri":"your_facebook_activity/posts/media/a/1.jpg","description":"beach"}}]}],"data":[]},
	 {"timestamp":1270000000,"title":"Me Self shared a link.","attachments":[{"data":[{"external_context":{"url":"https://example.com"}}]}],"data":[{"post":"` + strings.Repeat("long ", 20) + `"}]}]`
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	posts, err := ReadPosts(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 3 {
		t.Fatalf("read %d posts", len(posts))
	}
	if title := posts[0].Title("Me Self"); title != "café day # one" {
		t.Errorf("title %q", title)
	}
	body := posts[0].Body("entry_000009", map[string]string{"Zoë": "person_000007"})
	if !strings.Contains(body, "café day # one\n\n- _with_ [Zoë](ref:person_000007)\n\nMe Self updated their status. From the [Facebook export](ref:entry_000009).") {
		t.Errorf("body:\n%s", body)
	}
	if title := posts[1].Title("Me Self"); title != "Facebook: added a new photo" {
		t.Errorf("title %q", title)
	}
	if body := posts[1].Body("", nil); !strings.Contains(body, "- _photo: 1.jpg — beach_\n") {
		t.Errorf("photo body:\n%s", body)
	}
	if title := posts[2].Title("Me Self"); !strings.HasSuffix(title, "…") || len([]rune(title)) > titleLength+1 {
		t.Errorf("long title %q", title)
	}
}
