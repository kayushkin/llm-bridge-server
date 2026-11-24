package journalstore

import (
	"net/http"
	"strings"
	"testing"
)

func (s *testService) mustAddItems(timelineID string, items ...TimelineItemInput) []TimelineItem {
	s.t.Helper()
	var result TimelineItemsResult
	if status := s.call(http.MethodPost, "/timelines/"+timelineID+"/items", TimelineItemsRequest{Items: items}, &result); status != http.StatusCreated {
		s.t.Fatalf("POST /timelines/%s/items = %d: %+v", timelineID, status, result)
	}
	return result.Items
}

func TestATimelineMayNameWhatItFollowsAndOtherKindsStillMayNot(t *testing.T) {
	service := newTestService(t)
	person := service.mustCreate(EntryInput{Kind: "timeline", Title: "Carol", AboutRef: "person_000012"})
	if person.AboutRef != "person_000012" {
		t.Errorf("a timeline keeps its about_ref, got %q", person.AboutRef)
	}
	service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	var refused map[string]string
	if status := service.call(http.MethodPost, "/entries", EntryInput{Kind: "note", Title: "x", AboutRef: "person_000012"}, &refused); status != http.StatusBadRequest {
		t.Errorf("a note with about_ref = %d, want 400", status)
	}
}

func TestItemsAreWrittenAllOrNoneAndListedInTimeOrder(t *testing.T) {
	service := newTestService(t)
	timeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	items := service.mustAddItems(timeline.ID,
		TimelineItemInput{OccurredAt: 1800000000, Precision: "minute", Title: "landed"},
		TimelineItemInput{OccurredAt: 1700000000, EndsAt: 1700086400, Precision: "Day", Title: "  left  ", Sources: []string{"entry_000009", "entry_000009"}},
	)
	if items[0].ID != "timeline_item_000001" || items[1].Title != "left" || items[1].Precision != "day" {
		t.Fatalf("created = %+v", items)
	}
	if len(items[1].Sources) != 1 {
		t.Errorf("a repeated source is kept once, got %v", items[1].Sources)
	}

	bad := TimelineItemsRequest{Items: []TimelineItemInput{
		{OccurredAt: 1750000000, Precision: "day", Title: "fine"},
		{OccurredAt: 1750000000, Precision: "fortnight", Title: "not fine"},
	}}
	var refused map[string]string
	if status := service.call(http.MethodPost, "/timelines/"+timeline.ID+"/items", bad, &refused); status != http.StatusBadRequest {
		t.Fatalf("an unknown precision = %d, want 400", status)
	}
	if !strings.Contains(refused["error"], "item 1") {
		t.Errorf("the error should name the item, got %q", refused["error"])
	}

	var listed []TimelineItem
	service.call(http.MethodGet, "/timelines/"+timeline.ID+"/items", nil, &listed)
	if len(listed) != 2 || listed[0].Title != "left" || listed[1].Title != "landed" {
		t.Fatalf("listed = %+v, want the two good items, oldest first, and nothing of the refused request", listed)
	}

	for name, input := range map[string]TimelineItemInput{
		"no title":        {OccurredAt: 1, Precision: "day"},
		"no time":         {Precision: "day", Title: "x"},
		"ends before":     {OccurredAt: 10, EndsAt: 5, Precision: "day", Title: "x"},
		"source with gap": {OccurredAt: 10, Precision: "day", Title: "x", Sources: []string{"two ids"}},
	} {
		if status := service.call(http.MethodPost, "/timelines/"+timeline.ID+"/items", TimelineItemsRequest{Items: []TimelineItemInput{input}}, nil); status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, status)
		}
	}
}

func TestItemsGoOnlyOnALiveTimeline(t *testing.T) {
	service := newTestService(t)
	note := service.mustCreate(EntryInput{Kind: "note", Title: "not a timeline"})
	item := TimelineItemsRequest{Items: []TimelineItemInput{{OccurredAt: 1, Precision: "day", Title: "x"}}}
	if status := service.call(http.MethodPost, "/timelines/"+note.ID+"/items", item, nil); status != http.StatusBadRequest {
		t.Errorf("items on a note = %d, want 400", status)
	}
	if status := service.call(http.MethodPost, "/timelines/entry_999999/items", item, nil); status != http.StatusNotFound {
		t.Errorf("items on a missing entry = %d, want 404", status)
	}
}

func TestTheOverlayMergesTimelinesInTimeOrderWithinTheWindow(t *testing.T) {
	service := newTestService(t)
	trip := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	carol := service.mustCreate(EntryInput{Kind: "timeline", Title: "Carol", AboutRef: "person_000012"})
	other := service.mustCreate(EntryInput{Kind: "timeline", Title: "Not asked for"})
	service.mustAddItems(trip.ID, TimelineItemInput{OccurredAt: 300, Precision: "minute", Title: "trip 300"},
		TimelineItemInput{OccurredAt: 100, Precision: "minute", Title: "trip 100"})
	service.mustAddItems(carol.ID, TimelineItemInput{OccurredAt: 200, Precision: "minute", Title: "carol 200"},
		TimelineItemInput{OccurredAt: 400, Precision: "minute", Title: "carol 400"})
	service.mustAddItems(other.ID, TimelineItemInput{OccurredAt: 250, Precision: "minute", Title: "other 250"})

	var merged []TimelineItem
	if status := service.call(http.MethodGet, "/timeline-items?timeline="+trip.ID+"&timeline="+carol.ID+"&from=100&to=400", nil, &merged); status != http.StatusOK {
		t.Fatalf("overlay = %d", status)
	}
	var titles []string
	for _, item := range merged {
		titles = append(titles, item.Title)
	}
	if strings.Join(titles, ",") != "trip 100,carol 200,trip 300" {
		t.Errorf("overlay = %v, want trip 100, carol 200, trip 300 (from inclusive, to exclusive, other timelines left out)", titles)
	}
	if merged[1].TimelineID != carol.ID {
		t.Errorf("each item says which timeline it is on, got %q", merged[1].TimelineID)
	}
	if status := service.call(http.MethodGet, "/timeline-items", nil, nil); status != http.StatusBadRequest {
		t.Errorf("an overlay of nothing = %d, want 400", status)
	}
	note := service.mustCreate(EntryInput{Kind: "note", Title: "n"})
	if status := service.call(http.MethodGet, "/timeline-items?timeline="+note.ID, nil, nil); status != http.StatusBadRequest {
		t.Errorf("an overlay of a note = %d, want 400", status)
	}
}

func TestLinksAndSourcesInItemsAreMentionsTheBacklinksFind(t *testing.T) {
	service := newTestService(t)
	timeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip", Body: "with [Carol](ref:person_000012)"})
	items := service.mustAddItems(timeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "met Bob",
		Body: "[Bob](ref:person_000013) came", Sources: []string{"entry_000042"}})

	var backlinks []Backlink
	service.call(http.MethodGet, "/mentions?ref=person_000013", nil, &backlinks)
	if len(backlinks) != 1 || backlinks[0].Entry.ID != timeline.ID {
		t.Fatalf("backlinks of a person named only in an item = %+v, want the timeline", backlinks)
	}
	if mention := backlinks[0].Mentions; len(mention) != 1 || mention[0].ItemID != items[0].ID || mention[0].EntityType != "person" {
		t.Errorf("the mention should name the item and be typed, got %+v", mention)
	}
	service.call(http.MethodGet, "/mentions?ref=entry_000042", nil, &backlinks)
	if len(backlinks) != 1 || backlinks[0].Mentions[0].Label != "" {
		t.Errorf("a source is a mention with no label, got %+v", backlinks)
	}

	var entry Entry
	service.call(http.MethodGet, "/entries/"+timeline.ID, nil, &entry)
	if len(entry.Mentions) != 3 || entry.Mentions[0].ItemID != "" || entry.Mentions[1].ItemID != items[0].ID {
		t.Errorf("a timeline's mentions are its body's, then its items', got %+v", entry.Mentions)
	}

	service.call(http.MethodDelete, "/timeline-items/"+items[0].ID, nil, nil)
	service.call(http.MethodGet, "/mentions?ref=person_000013", nil, &backlinks)
	if len(backlinks) != 0 {
		t.Errorf("a deleted item's links are not backlinks, got %+v", backlinks)
	}
}

func TestItemPatchDeleteAndRestoreKeepEveryState(t *testing.T) {
	service := newTestService(t)
	timeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	item := service.mustAddItems(timeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "first"})[0]

	var patched TimelineItem
	if status := service.call(http.MethodPatch, "/timeline-items/"+item.ID, map[string]any{"title": "second", "sources": []string{"entry_000001"}}, &patched); status != http.StatusOK {
		t.Fatalf("PATCH = %d", status)
	}
	if patched.Title != "second" || len(patched.Sources) != 1 {
		t.Errorf("patched = %+v", patched)
	}
	for body, want := range map[string]int{`{"titel":"x"}`: 400, `{"timeline_id":"entry_000002"}`: 400, `{"title":null}`: 400, `{"precision":"week"}`: 400} {
		if status := service.call(http.MethodPatch, "/timeline-items/"+item.ID, body, nil); status != want {
			t.Errorf("PATCH %s = %d, want %d", body, status, want)
		}
	}

	if status := service.call(http.MethodDelete, "/timeline-items/"+item.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	if status := service.call(http.MethodGet, "/timeline-items/"+item.ID, nil, nil); status != http.StatusNotFound {
		t.Errorf("a deleted item = %d, want 404", status)
	}
	if status := service.call(http.MethodPatch, "/timeline-items/"+item.ID, map[string]any{"title": "x"}, nil); status != http.StatusNotFound {
		t.Errorf("PATCH of a deleted item = %d, want 404", status)
	}
	var restored TimelineItem
	service.call(http.MethodPost, "/timeline-items/"+item.ID+"/restore", nil, &restored)
	if restored.Title != "second" || restored.DeletedAt != 0 {
		t.Errorf("restored = %+v", restored)
	}

	var revisions []TimelineItemRevision
	service.call(http.MethodGet, "/timeline-items/"+item.ID+"/revisions", nil, &revisions)
	var reasons []string
	for _, revision := range revisions {
		reasons = append(reasons, revision.Reason+":"+revision.Item.Title)
	}
	if strings.Join(reasons, ",") != "restore:second,delete:second,update:first" {
		t.Errorf("revisions = %v, want restore, delete, update, newest first, each the state before", reasons)
	}
}

func TestATimelineWithItemsKeepsItsKind(t *testing.T) {
	service := newTestService(t)
	timeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	item := service.mustAddItems(timeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "x"})[0]
	if status := service.call(http.MethodPatch, "/entries/"+timeline.ID, map[string]any{"kind": "note"}, nil); status != http.StatusConflict {
		t.Errorf("making a timeline with items a note = %d, want 409", status)
	}
	service.call(http.MethodDelete, "/timeline-items/"+item.ID, nil, nil)
	if status := service.call(http.MethodPatch, "/entries/"+timeline.ID, map[string]any{"kind": "note"}, nil); status != http.StatusOK {
		t.Errorf("once its items are deleted it may change kind; got %d", status)
	}
}

func TestPrecisionsAreServedAndHealthCountsItems(t *testing.T) {
	service := newTestService(t)
	var precisions []string
	service.call(http.MethodGet, "/time-precisions", nil, &precisions)
	if strings.Join(precisions, ",") != "minute,day,month,year" {
		t.Errorf("precisions = %v", precisions)
	}
	timeline := service.mustCreate(EntryInput{Kind: "timeline", Title: "A trip"})
	service.mustAddItems(timeline.ID, TimelineItemInput{OccurredAt: 100, Precision: "day", Title: "x"})
	var health Health
	service.call(http.MethodGet, "/health", nil, &health)
	if health.Counts.TimelineItems != 1 {
		t.Errorf("health counts %d timeline items, want 1", health.Counts.TimelineItems)
	}
}
