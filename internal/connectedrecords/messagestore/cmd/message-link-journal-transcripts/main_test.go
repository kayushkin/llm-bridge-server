package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	messagestore "github.com/kayushkin/message-store"
)

func TestLinkPagesThroughJournalStoreAndPostsEveryTranscript(t *testing.T) {
	journal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("kind") != "transcript" || r.URL.Query().Get("source") != "skype" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		var page []journalEntry
		if r.URL.Query().Get("offset") == "0" {
			for i := 0; i < pageSize; i++ {
				page = append(page, journalEntry{ID: fmt.Sprintf("entry_%06d", i+1), Source: "skype", SourceRef: fmt.Sprintf("carol_c/day-%d", i)})
			}
		} else {
			page = []journalEntry{{ID: "entry_999999", Source: "skype", SourceRef: "carol_c/last"}}
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer journal.Close()
	var posted []messagestore.JournalEntryLink
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var links []messagestore.JournalEntryLink
		json.NewDecoder(r.Body).Decode(&links)
		posted = append(posted, links...)
		json.NewEncoder(w).Encode(messagestore.JournalEntryLinkResult{Linked: len(links) - 1, Unmatched: links[len(links)-1:]})
	}))
	defer store.Close()
	l := &linker{journalURL: journal.URL, storeURL: store.URL, http: http.DefaultClient}
	var out strings.Builder
	if err := l.link("skype", false, 5, &out); err != nil {
		t.Fatal(err)
	}
	if len(posted) != pageSize+1 || posted[pageSize].JournalEntryID != "entry_999999" {
		t.Errorf("posted %d links", len(posted))
	}
	if !strings.Contains(out.String(), "skype: 501 transcripts, 500 linked, 1 unmatched") {
		t.Errorf("output = %q", out.String())
	}
}
