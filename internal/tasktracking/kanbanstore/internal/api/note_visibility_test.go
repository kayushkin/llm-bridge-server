package api_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// An agent writes "customer is wrong about the invoice, do not concede" on a
// ticket, and then a reply for the customer. A requester-facing reader must be
// able to get the second without the first, from the notes and from the
// timeline, whose note_added events carry each note's text as their summary.
func TestANoteSaysWhoItIsForAndARequesterFacingReaderGetsOnlyTheirs(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	notesPath := "/api/cards/" + f.supportCardID + "/notes"

	write := func(body string, visibility string, want int) model.CardNote {
		t.Helper()
		request := map[string]any{"body": body}
		if visibility != "" {
			request["visibility"] = visibility
		}
		w := requestAs(t, h, asPrincipal(alice), "POST", notesPath, request)
		mustStatus(t, w, want, "note "+body)
		var note model.CardNote
		if want == 201 {
			decodeSuccessfulResponse(t, w, &note)
		}
		return note
	}
	unmarked := write("do not concede on the invoice", "", 201)
	if unmarked.Visibility != model.NoteVisibilityInternal {
		t.Fatalf("a note written with no visibility is %q; it must be internal, or a field left out shows a requester something", unmarked.Visibility)
	}
	write("we are looking into the invoice", "requester", 201)
	write("shown to whom?", "everyone", 400)

	bodiesOf := func(query string) []string {
		w := requestAs(t, h, asPrincipal(alice), "GET", notesPath+query, nil)
		mustStatus(t, w, 200, "notes"+query)
		var notes []model.CardNote
		decodeSuccessfulResponse(t, w, &notes)
		bodies := []string{}
		for _, note := range notes {
			bodies = append(bodies, note.Body)
		}
		return bodies
	}
	if got, want := bodiesOf("?visibility=requester"), []string{"we are looking into the invoice"}; !reflect.DeepEqual(got, want) {
		t.Errorf("requester notes = %v, want %v", got, want)
	}
	if got, want := bodiesOf("?visibility=internal"), []string{"do not concede on the invoice"}; !reflect.DeepEqual(got, want) {
		t.Errorf("internal notes = %v, want %v", got, want)
	}
	if got := bodiesOf(""); len(got) != 2 {
		t.Errorf("with no filter the people working the card see %v, want both", got)
	}
	// A filter that cannot be read must not fall back to "every note".
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", notesPath+"?visibility=public", nil), 400, "an unknown visibility filter")

	// The timeline carries the text, so each note_added event says who it is for.
	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.supportCardID+"/events", nil)
	mustStatus(t, w, 200, "events")
	var events []model.CardEvent
	decodeSuccessfulResponse(t, w, &events)
	visibilityBySummary := map[string]model.NoteVisibility{}
	for _, event := range events {
		if event.Kind != model.EventNoteAdded {
			continue
		}
		var detail model.NoteAddedEventDetail
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			t.Fatalf("note_added event %q has detail %s: %v", event.Summary, event.Detail, err)
		}
		visibilityBySummary[event.Summary] = detail.Visibility
	}
	want := map[string]model.NoteVisibility{
		"do not concede on the invoice":   model.NoteVisibilityInternal,
		"we are looking into the invoice": model.NoteVisibilityRequester,
	}
	if !reflect.DeepEqual(visibilityBySummary, want) {
		t.Errorf("note_added events say %v, want %v", visibilityBySummary, want)
	}

	w = requestAs(t, h, asPrincipal(bob), "GET", "/api/note-visibilities", nil)
	mustStatus(t, w, 200, "the vocabulary, which names no board")
	var vocabulary []model.NoteVisibility
	decodeSuccessfulResponse(t, w, &vocabulary)
	if !reflect.DeepEqual(vocabulary, model.NoteVisibilities) {
		t.Errorf("vocabulary = %v", vocabulary)
	}
}
