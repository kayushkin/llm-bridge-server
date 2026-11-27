package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledger"
)

func entryWith(id, outcome string, kinds []string, links ...timeledger.Link) timeledger.TimeEntry {
	entry := timeledger.TimeEntry{ID: id, Outcome: outcome, Links: links}
	for _, kind := range kinds {
		entry.Kinds = append(entry.Kinds, timeledger.KindAssignment{Name: kind})
	}
	return entry
}

func source(linkType, ref string) timeledger.Link {
	return timeledger.Link{LinkType: linkType, Ref: ref, BearsOn: timeledger.BearsOnSource}
}

func TestCountAttendanceByKindSplitsSeriesDatesFromOneOffs(t *testing.T) {
	removed := int64(9)
	removedSource := source("calendar_events", "calendar_event_000003")
	removedSource.RemovedAt = &removed
	entries := []timeledger.TimeEntry{
		// Two dates of one weekly class: series dates.
		entryWith("time_entry_000001", "done", []string{"dance", "class"}, source("calendar_events", "calendar_event_000001")),
		entryWith("time_entry_000002", "skipped", []string{"dance", "class"}, source("calendar_events", "calendar_event_000001")),
		// A one-off with the same ref under another link type is not the same record.
		entryWith("time_entry_000003", "skipped", []string{"dance"}, source("discord_events", "calendar_event_000001")),
		// A removed source link shared with a live one does not make a series.
		entryWith("time_entry_000004", "unknown", nil, source("calendar_events", "calendar_event_000002")),
		entryWith("time_entry_000005", "done", []string{"social"}, removedSource, source("calendar_events", "calendar_event_000004")),
		entryWith("time_entry_000006", "done", []string{"social"}, source("calendar_events", "calendar_event_000003")),
		// A context link is not a source.
		entryWith("time_entry_000007", "done", []string{"social"},
			timeledger.Link{LinkType: "calendar_events", Ref: "calendar_event_000004", BearsOn: timeledger.BearsOnContext}),
	}
	got := map[string]KindAttendance{}
	for _, attendance := range countAttendanceByKind(entries) {
		got[attendance.Kind] = attendance
	}
	want := map[string]KindAttendance{
		"class":  {Kind: "class", SeriesDates: OutcomeCounts{Done: 1, Skipped: 1}},
		"dance":  {Kind: "dance", OneOff: OutcomeCounts{Skipped: 1}, SeriesDates: OutcomeCounts{Done: 1, Skipped: 1}},
		noKind:   {Kind: noKind, OneOff: OutcomeCounts{Unknown: 1}},
		"social": {Kind: "social", OneOff: OutcomeCounts{Done: 3}},
	}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for kind, expected := range want {
		if got[kind] != expected {
			t.Errorf("%s = %+v, want %+v", kind, got[kind], expected)
		}
	}
}

func TestPrintReportOrdersCategoriesByCountAndNamesTheBlankOne(t *testing.T) {
	var out bytes.Buffer
	printReport(&out, []KindAttendance{{Kind: "dance", OneOff: OutcomeCounts{Done: 2}}},
		map[string]int{"Dance": 3, "Show": 7, "": 1}, "candidate", 2)
	text := out.String()
	show, dance, blank := strings.Index(text, "Show"), strings.Index(text, "Dance"), strings.Index(text, "(no category)")
	if show < 0 || dance < 0 || blank < 0 || !(show < dance && dance < blank) {
		t.Errorf("categories not ordered by count, or the blank one unnamed:\n%s", text)
	}
	if !strings.Contains(text, "2 live time entries") || !strings.Contains(text, `status "candidate"`) {
		t.Errorf("header lines missing:\n%s", text)
	}
}
