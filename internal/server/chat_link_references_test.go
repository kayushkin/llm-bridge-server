package server

import (
	"context"
	"regexp"
	"testing"
)

func TestAReplyDrawsOnlyTheRecordsItsRecipientMaySee(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)\b(?:(?:prediction_\d{6,})|(?:entry_\d{6,})|(?:music_song_\d{6,})|(?:photo_\d{6,}))\b`)
	var asked []string
	resolve := func(_ context.Context, ids []string) (map[string][]referenceMatch, error) {
		asked = ids
		return map[string][]referenceMatch{
			"prediction_000012": {{Label: "It [rains] tomorrow", PageURL: "/predictions?id=prediction_000012"}},
			"entry_000001":      {{Refused: true}},
			"music_song_000001": {{Label: "A song", PageURL: "https://music.example/music/records/music_song_000001"}},
			"photo_000009":      {{Label: ""}},
		}, nil
	}
	body := "See prediction_000012 and entry_000001, then music_song_000001, photo_000009, " +
		"`prediction_000012` in code, [prediction_000012](https://x.example) linked, and prediction_000012 again."
	got, err := drawReferencesForRecipient(context.Background(), body, pattern, "https://dash.example/", resolve)
	if err != nil {
		t.Fatal(err)
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
	got, err := drawReferencesForRecipient(context.Background(), "x_000001", pattern, "https://dash.example", resolve)
	if err != nil || got != "x_000001" {
		t.Errorf("%q %v", got, err)
	}
}
