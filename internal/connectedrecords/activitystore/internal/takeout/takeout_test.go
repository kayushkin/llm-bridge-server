package takeout

import (
	"os"
	"strings"
	"testing"
	"time"

	activitystore "github.com/kayushkin/activity-store"
)

func utc(year int, month time.Month, day, hour, minute, second int) int64 {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC).Unix()
}

// An abbreviation is its fixed offset whatever the date: Takeout writes a
// January time with the summer abbreviation it had when exported.
func TestMyActivityTimesReadTheAbbreviationAsAFixedOffset(t *testing.T) {
	cases := []struct {
		text string
		want int64
	}{
		{"Jan 5, 2026, 7:01:10 PM PDT", utc(2026, 1, 6, 2, 1, 10)},
		{"Jul 4, 2025, 12:00:00 AM PST", utc(2025, 7, 4, 8, 0, 0)},
		{"Dec 31, 2019, 11:59:59 PM UTC", utc(2019, 12, 31, 23, 59, 59)},
		{"Mar 3, 2021, 1:02:03 PM GMT+05:30", utc(2021, 3, 3, 7, 32, 3)},
		{"Sept 9, 2022, 9:09:09 AM CEST", utc(2022, 9, 9, 7, 9, 9)},
	}
	for _, c := range cases {
		got, err := ParseMyActivityTime(c.text)
		if err != nil {
			t.Fatalf("%q: %v", c.text, err)
		}
		if got != c.want {
			t.Errorf("%q = %s, want %s", c.text, time.Unix(got, 0).UTC(), time.Unix(c.want, 0).UTC())
		}
	}
	for _, refused := range []string{"Jan 5, 2026, 7:01:10 PM IST", "5 Jan 2026 19:01", "Jan 5, 2026"} {
		if _, err := ParseMyActivityTime(refused); err == nil {
			t.Errorf("%q was read; it should be refused", refused)
		}
	}
}

func readFixture(t *testing.T) []Record {
	t.Helper()
	file, err := os.Open("testdata/MyActivity.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	records, err := ReadMyActivity(file, MyActivityPage{Product: "Search", Source: activitystore.SourceGoogleMyActivity})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 8 {
		t.Fatalf("read %d records, want the fixture's 8", len(records))
	}
	return records
}

func TestAMyActivityCardBecomesAnActivityWithItsDetails(t *testing.T) {
	records := readFixture(t)
	search := records[0]
	if search.Activity.Action != activitystore.ActionSearched || search.Activity.ActionText != "Searched for" ||
		search.Activity.Title != "sourdough starter" || search.Activity.URL != "https://www.google.com/search?q=sourdough+starter" {
		t.Errorf("search = %+v", search.Activity)
	}
	if search.Activity.OccurredAt != utc(2026, 1, 6, 2, 1, 10) {
		t.Errorf("occurred_at = %d", search.Activity.OccurredAt)
	}
	if search.Activity.Latitude == nil || *search.Activity.Latitude != 10.0001 || *search.Activity.Longitude != 20.0002 {
		t.Errorf("latitude, longitude = %v, %v; want the Locations line's", search.Activity.Latitude, search.Activity.Longitude)
	}
	if len(search.Activity.Details.Locations) != 1 || search.Activity.Details.Locations[0].Name != "Example Town" ||
		search.Activity.Details.Locations[0].Basis != "From your device" {
		t.Errorf("locations = %+v", search.Activity.Details.Locations)
	}
	if len(search.Activity.Details.Products) != 1 || search.Activity.Details.Products[0] != "Search" || search.Activity.Details.Header != "" {
		t.Errorf("products = %v, header = %q", search.Activity.Details.Products, search.Activity.Details.Header)
	}
	if len(search.AttachmentFiles) != 1 || search.AttachmentFiles[0] != "2026-01-06_02_01_10_000_UTC.mp3" {
		t.Errorf("attachments = %v", search.AttachmentFiles)
	}

	watch := records[1]
	if watch.Activity.Action != activitystore.ActionWatched || watch.Activity.Details.Channel == nil ||
		watch.Activity.Details.Channel.Name != "Example Channel" || len(watch.Activity.Details.Lines) != 0 {
		t.Errorf("watch = %+v, channel %+v", watch.Activity, watch.Activity.Details.Channel)
	}
	if len(watch.Activity.Details.Notes) != 1 || watch.Activity.Details.Notes[0] != "From Google Ads" {
		t.Errorf("notes = %v", watch.Activity.Details.Notes)
	}
	if watch.Activity.Details.Header != "YouTube" {
		t.Errorf("a header other than the page's product is kept: %q", watch.Activity.Details.Header)
	}

	directions := records[2]
	if directions.Activity.Action != activitystore.ActionDirections || len(directions.Activity.Details.Lines) != 2 ||
		directions.Activity.Details.Lines[0].Text != "Driving directions" || directions.Activity.Latitude != nil {
		t.Errorf("directions = %+v", directions.Activity)
	}

	bare := records[3]
	if bare.Activity.Action != activitystore.ActionViewed || bare.Activity.ActionText != "" || bare.Activity.Title != "Example Park" {
		t.Errorf("a card that is only a link = %+v, want viewed", bare.Activity)
	}

	used := records[5]
	if used.Activity.Action != activitystore.ActionUsed || used.Activity.Title != "com.example.notes" || used.Activity.URL != "" {
		t.Errorf("a card with no link = %+v", used.Activity)
	}

	prompt := records[6]
	if prompt.Activity.Action != activitystore.ActionPrompted || prompt.Activity.Title != "what rhymes with orange" {
		t.Errorf("prompt = %+v", prompt.Activity)
	}
	var answer []string
	for _, line := range prompt.Activity.Details.Lines {
		answer = append(answer, line.Text)
	}
	if strings.Join(answer, "|") != "Nothing quite does.|door hinge|sporange" {
		t.Errorf("the answer after the time = %q", answer)
	}

	explored := records[7]
	if explored.Activity.Action != activitystore.ActionOther || explored.Activity.Title != "Explored on Google Maps" ||
		explored.Activity.Latitude != nil || len(explored.Activity.Details.Locations) != 1 {
		t.Errorf("explored = %+v", explored.Activity)
	}
}

// Two identical cards are two records with references that differ, and each
// reference is the same when the page is read again.
func TestReferencesAreStableAndDistinct(t *testing.T) {
	first, second := readFixture(t), readFixture(t)
	seen := map[string]bool{}
	for index := range first {
		if first[index].Activity.SourceRef != second[index].Activity.SourceRef {
			t.Errorf("record %d's reference changed between reads", index)
		}
		if seen[first[index].Activity.SourceRef] {
			t.Errorf("record %d shares a reference", index)
		}
		seen[first[index].Activity.SourceRef] = true
	}
	if !strings.HasSuffix(first[4].Activity.SourceRef, "#2") {
		t.Errorf("the repeated card's reference = %q, want a #2 suffix", first[4].Activity.SourceRef)
	}
}

func TestChromeHistoryVisitsKeepTheSecond(t *testing.T) {
	records, err := ReadChromeHistory(strings.NewReader(`{"Browser History":[{"title":"Example","url":"https://example.com/a",
		"time_usec":1700000000123456,"client_id":"client-a","page_transition_qualifier":"CLIENT_REDIRECT","favicon_url":""}],"Session":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Activity.OccurredAt != 1700000000 || records[0].Activity.Action != activitystore.ActionVisited ||
		records[0].Activity.Details.ChromeClientID != "client-a" || !strings.HasPrefix(records[0].Activity.SourceRef, "1700000000123456-") {
		t.Errorf("records = %+v", records)
	}
	if _, err := ReadChromeHistory(strings.NewReader(`{"Session":[]}`)); err == nil {
		t.Error("a file without Browser History was read")
	}
}

func TestBookmarksCarryTheirFolderAndSkipTheUndated(t *testing.T) {
	read, err := ReadChromeBookmarks(strings.NewReader(`<!DOCTYPE NETSCAPE-Bookmark-file-1>
<DL><p>
<DT><H3 ADD_DATE="0" LAST_MODIFIED="0" PERSONAL_TOOLBAR_FOLDER="true">Bookmarks bar</H3>
<DL><p>
<DT><H3 ADD_DATE="1600000000">Recipes</H3>
<DL><p>
<DT><A HREF="https://example.com/bread" ADD_DATE="1600000100">Bread</A>
</DL><p>
<DT><A HREF="https://example.com/old" ADD_DATE="0">Old</A>
</DL><p>
</DL><p>`))
	if err != nil {
		t.Fatal(err)
	}
	if read.Undated != 1 || len(read.Records) != 1 {
		t.Fatalf("read %+v", read)
	}
	bookmark := read.Records[0].Activity
	if bookmark.Title != "Bread" || bookmark.OccurredAt != 1600000100 || bookmark.Details.BookmarkFolder != "Bookmarks bar / Recipes" {
		t.Errorf("bookmark = %+v", bookmark)
	}
}
