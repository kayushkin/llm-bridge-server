// Command journal-point-transcripts-at-messages replaces the body of each
// transcript entry with a link to the conversation day message-store now
// holds, one message per row. The entry keeps its id, so every timeline item
// and note that cites it still lands on the conversation; its old body stays
// in the entry's revisions.
//
// An entry is rewritten only when message-store has a conversation day linked
// to it that holds at least one message. Every person and photo the old body
// linked is kept as a link in the new one, so GET /mentions still finds the
// entry from them. A rewritten entry gets the tag moved-to-message-store and is
// skipped by the next run.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	journalstore "github.com/kayushkin/journal-store"
)

// MovedTag marks an entry whose body now points at message-store.
const MovedTag = "moved-to-message-store"

var client = &http.Client{Timeout: time.Minute}

// keptRef matches the person and photo links an old transcript body made.
var keptRef = regexp.MustCompile(`\[([^\]]*)\]\(ref:((?:person|photo)_\d{6,})\)`)

type conversationDay struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Day            string `json:"day"`
	MessageCount   int    `json:"message_count"`
}

type conversation struct {
	Platform string `json:"platform"`
	Title    string `json:"title"`
}

func getJSON(address string, into any) error {
	response, err := client.Get(address)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s: %s", address, response.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, into)
}

func patchEntry(storeURL, id string, patch journalstore.EntryPatch) error {
	payload, _ := json.Marshal(patch)
	request, _ := http.NewRequest(http.MethodPatch, storeURL+"/entries/"+id, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("PATCH /entries/%s: %s: %s", id, response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// pointerBody is the new body: the conversation day, then the people and
// pictures the old body linked.
func pointerBody(day conversationDay, chat conversation, oldBody string) string {
	var builder strings.Builder
	label := chat.Title
	if label == "" {
		label = "the conversation"
	}
	fmt.Fprintf(&builder, "This day's messages are in message-store: [%s on %s](ref:%s), %d messages on %s ([whole conversation](ref:%s)).\n",
		label, day.Day, day.ID, day.MessageCount, chat.Platform, day.ConversationID)
	seen := map[string]bool{}
	var people, photos []string
	for _, match := range keptRef.FindAllStringSubmatch(oldBody, -1) {
		text, ref := match[1], match[2]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if strings.HasPrefix(ref, "person_") {
			people = append(people, fmt.Sprintf("[%s](ref:%s)", text, ref))
		} else {
			if text == "" {
				text = "picture"
			}
			photos = append(photos, fmt.Sprintf("[%s](ref:%s)", text, ref))
		}
	}
	if len(people) > 0 {
		fmt.Fprintf(&builder, "\nWith %s.\n", strings.Join(people, ", "))
	}
	if len(photos) > 0 {
		fmt.Fprintf(&builder, "\nPictures: %s.\n", strings.Join(photos, ", "))
	}
	return builder.String()
}

func main() {
	journalURL := flag.String("journal-store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	messageURL := flag.String("message-store-url", "http://127.0.0.1:8337", "message-store's base URL")
	source := flag.String("source", "", "only entries imported under this source (skype, discord-package…); empty: every transcript")
	dryRun := flag.Bool("dry-run", false, "count what would change and write nothing")
	limit := flag.Int("limit", 0, "stop after rewriting this many entries; 0: no limit")
	flag.Parse()
	*journalURL, *messageURL = strings.TrimRight(*journalURL, "/"), strings.TrimRight(*messageURL, "/")

	var entries []journalstore.Entry
	for offset := 0; ; offset += 500 {
		query := url.Values{"kind": {"transcript"}, "limit": {"500"}, "offset": {fmt.Sprint(offset)}}
		if *source != "" {
			query.Set("source", *source)
		}
		var page []journalstore.Entry
		if err := getJSON(*journalURL+"/entries?"+query.Encode(), &page); err != nil {
			log.Fatal(err)
		}
		entries = append(entries, page...)
		if len(page) < 500 {
			break
		}
	}

	conversations := map[string]conversation{}
	counts := map[string]int{}
	var unlinked []string
	for _, listed := range entries {
		if *limit > 0 && counts["rewritten"] >= *limit {
			break
		}
		if contains(listed.Tags, MovedTag) {
			counts["already moved"]++
			continue
		}
		var days []conversationDay
		if err := getJSON(*messageURL+"/conversation-days?journal_entry="+url.QueryEscape(listed.ID), &days); err != nil {
			log.Fatal(err)
		}
		if len(days) != 1 || days[0].MessageCount == 0 {
			counts["not linked or empty"]++
			unlinked = append(unlinked, listed.ID)
			continue
		}
		day := days[0]
		chat, known := conversations[day.ConversationID]
		if !known {
			if err := getJSON(*messageURL+"/conversations/"+day.ConversationID, &chat); err != nil {
				log.Fatal(err)
			}
			conversations[day.ConversationID] = chat
		}
		// The list leaves bodies out of nothing, but read the entry itself so
		// the body replaced is the one stored now.
		var entry journalstore.Entry
		if err := getJSON(*journalURL+"/entries/"+listed.ID, &entry); err != nil {
			log.Fatal(err)
		}
		body := pointerBody(day, chat, entry.Body)
		tags := append(append([]string{}, entry.Tags...), MovedTag)
		counts["rewritten"]++
		if *dryRun {
			continue
		}
		if err := patchEntry(*journalURL, entry.ID, journalstore.EntryPatch{Body: &body, Tags: &tags}); err != nil {
			log.Fatal(err)
		}
	}
	verb := "rewrote"
	if *dryRun {
		verb = "would rewrite"
	}
	fmt.Printf("%d transcript entries: %s %d, %d already moved, %d left alone (no conversation day with messages)\n",
		len(entries), verb, counts["rewritten"], counts["already moved"], counts["not linked or empty"])
	for i, id := range unlinked {
		if i == 20 {
			fmt.Printf("… and %d more\n", len(unlinked)-20)
			break
		}
		fmt.Println("left alone:", id)
	}
	if len(unlinked) > 0 {
		os.Exit(2)
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
