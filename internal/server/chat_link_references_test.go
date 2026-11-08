package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestAReplyDrawsOnlyTheRecordsItsRecipientMaySee(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)\b(?:(?:prediction_\d{6,})|(?:entry_\d{6,})|(?:music_song_\d{6,})|(?:photo_\d{6,}))\b`)
	var asked []string
	resolve := func(_ context.Context, ids []string) (map[string][]referenceMatch, error) {
		asked = ids
		return map[string][]referenceMatch{
			"prediction_000012": {{Type: "prediction", Label: "It [rains] tomorrow", PageURL: "/predictions?id=prediction_000012"}},
			"entry_000001":      {{Refused: true}},
			"music_song_000001": {{Label: "A song", PageURL: "https://music.example/music/records/music_song_000001"}},
			"photo_000009":      {{Type: "photo", Label: ""}},
		}, nil
	}
	body := "See prediction_000012 and entry_000001, then music_song_000001, photo_000009, " +
		"`prediction_000012` in code, [prediction_000012](https://x.example) linked, and prediction_000012 again."
	got, linked, err := drawReferencesForRecipient(context.Background(), body, pattern, "https://dash.example/", resolve)
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) != 3 || linked[0] != (linkedReference{ID: "prediction_000012", Type: "prediction"}) || linked[2] != (linkedReference{ID: "photo_000009", Type: "photo"}) {
		t.Errorf("linked: %+v", linked)
	}
	want := "See [It (rains) tomorrow](https://dash.example/predictions?id=prediction_000012) and entry_000001, then " +
		"[A song](https://music.example/music/records/music_song_000001), [photo_000009](https://dash.example/ref/photo_000009), " +
		"`prediction_000012` in code, [prediction_000012](https://x.example) linked, and " +
		"[It (rains) tomorrow](https://dash.example/predictions?id=prediction_000012) again."
	if got != want {
		t.Errorf("drawn:\n%s\nwant:\n%s", got, want)
	}
	if len(asked) != 4 {
		t.Errorf("asked about %v: each id once, and none from a protected span", asked)
	}
}

func TestWithNoSiteOnlyALinkToItsOwnSiteIsDrawn(t *testing.T) {
	if got := referencePageForChat("/predictions?id=x", "x", ""); got != "" {
		t.Errorf("a page on this site with no base: %q", got)
	}
	if got := referencePageForChat("https://music.example/r/x", "x", ""); got != "https://music.example/r/x" {
		t.Errorf("a page on its own site: %q", got)
	}
	if got := referencePageForChat("", "repo:dash", "https://dash.example"); got != "https://dash.example/ref/repo:dash" {
		t.Errorf("the generic record page: %q", got)
	}
}

func TestAnIdTwoStoresClaimStaysAsWritten(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)\b(?:x_\d{6,})\b`)
	resolve := func(context.Context, []string) (map[string][]referenceMatch, error) {
		return map[string][]referenceMatch{"x_000001": {{Label: "a", PageURL: "/a"}, {Label: "b", PageURL: "/b"}}}, nil
	}
	got, _, err := drawReferencesForRecipient(context.Background(), "x_000001", pattern, "https://dash.example", resolve)
	if err != nil || got != "x_000001" {
		t.Errorf("%q %v", got, err)
	}
}

func TestAPictureIsFetchedAsTheRecipientAndNamedByItsType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(principalIdentityHeader) != "principal_000009" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/attachments/photo_000009":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xff, 0xd8, 0xff})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	file, found, err := pictureOfReferenceAs(context.Background(), http.DefaultClient, server.URL, "principal_000009", "photo_000009")
	if err != nil || !found || file.ContentType != "image/jpeg" || !strings.HasPrefix(file.Name, "photo_000009.") || file.DataBase64 != "/9j/" {
		t.Fatalf("picture: %+v %v %v", file, found, err)
	}
	if _, found, err := pictureOfReferenceAs(context.Background(), http.DefaultClient, server.URL, "principal_000004", "photo_000009"); err != nil || found {
		t.Fatalf("someone else: %v %v", found, err)
	}
}
