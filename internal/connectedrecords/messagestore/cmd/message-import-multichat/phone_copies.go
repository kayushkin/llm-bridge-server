package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	messagestore "github.com/kayushkin/message-store"
)

// Texts copied off the user's phones (message-import-android, source sms)
// and texts multichat carried overlap where both cover the same weeks. The
// phone copy is kept: it is the archive the phones hold, filed by number. A
// multichat text is the phone text when their text is equal and the times
// agree: to the millisecond for the Google Messages bridge, to the minute for
// the web bridge, whose times are cut to the minute. Each multichat room is
// held to the one phone conversation most of its matches fall in, so a
// common text sent to someone else in the same minute is not taken for it.
// The multichat row is not sent and is retired in favor of the phone row,
// with the rows it would itself have superseded.

// phoneSource is the id namespace message-import-android files texts under.
const phoneSource = "sms"

// phoneText is one stored phone text.
type phoneText struct {
	ID, ConversationID, SourceRef, Body string
	SentAt                              int64
}

// readPhoneTexts reads every text the phone importer stored.
func readPhoneTexts(storeURL string) ([]phoneText, error) {
	var texts []phoneText
	cursor := ""
	for {
		query := url.Values{"platform": {"sms"}, "limit": {"5000"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Messages []struct {
				ID             string `json:"id"`
				ConversationID string `json:"conversation_id"`
				SentAt         int64  `json:"sent_at"`
				Kind           string `json:"kind"`
				Body           string `json:"body"`
				Source         string `json:"source"`
				SourceRef      string `json:"source_ref"`
			} `json:"messages"`
			Next string `json:"next"`
		}
		found, err := getJSON(strings.TrimRight(storeURL, "/")+"/messages?"+query.Encode(), nil, &page)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("message-store answered GET /messages with 404")
		}
		for _, message := range page.Messages {
			if message.Source == phoneSource && message.Body != "" && message.Kind != "call" {
				texts = append(texts, phoneText{ID: message.ID, ConversationID: message.ConversationID, SourceRef: message.SourceRef, Body: message.Body, SentAt: message.SentAt})
			}
		}
		if page.Next == "" {
			return texts, nil
		}
		cursor = page.Next
	}
}

// phoneCopies are the multichat messages that are phone texts.
type phoneCopies struct {
	// phoneOf is, by a multichat message's source_ref, the phone text it is.
	phoneOf map[string]phoneText
}

// matchPhoneCopies pairs multichat texts in Google Messages rooms with phone
// texts, one to one.
func matchPhoneCopies(messages []messagestore.ImportMessage, texts []phoneText) phoneCopies {
	type key struct {
		body string
		at   int64
	}
	exact, byMinute := map[key][]phoneText{}, map[key][]phoneText{}
	for _, text := range texts {
		exact[key{text.Body, text.SentAt}] = append(exact[key{text.Body, text.SentAt}], text)
		minute := time.UnixMilli(text.SentAt).Truncate(time.Minute).UnixMilli()
		byMinute[key{text.Body, minute}] = append(byMinute[key{text.Body, minute}], text)
	}
	type candidate struct {
		message messagestore.ImportMessage
		phone   []phoneText
	}
	var candidates []candidate
	// votes counts, per room, the phone conversations its candidates fall in.
	votes := map[string]map[string]int{}
	for _, message := range messages {
		if message.Source != SourceMultichat || message.Body == "" {
			continue
		}
		var phone []phoneText
		switch {
		case strings.HasPrefix(message.SenderHandle, "@"+platformGoogleMessagesWeb+"_"):
			phone = byMinute[key{message.Body, message.SentAt}]
		case strings.HasPrefix(message.SenderHandle, "@"+platformGoogleMessagesBridge+"_"):
			phone = exact[key{message.Body, message.SentAt}]
		}
		if len(phone) == 0 {
			continue
		}
		candidates = append(candidates, candidate{message, phone})
		room := message.ConversationSourceRef
		if votes[room] == nil {
			votes[room] = map[string]int{}
		}
		for _, text := range phone {
			votes[room][text.ConversationID]++
		}
	}
	chosen := map[string]string{}
	for room, counts := range votes {
		conversations := make([]string, 0, len(counts))
		for conversation := range counts {
			conversations = append(conversations, conversation)
		}
		sort.Slice(conversations, func(i, j int) bool {
			if counts[conversations[i]] != counts[conversations[j]] {
				return counts[conversations[i]] > counts[conversations[j]]
			}
			return conversations[i] < conversations[j]
		})
		chosen[room] = conversations[0]
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].message.SentAt < candidates[j].message.SentAt })
	copies := phoneCopies{phoneOf: map[string]phoneText{}}
	used := map[string]bool{}
	for _, c := range candidates {
		for _, text := range c.phone {
			if text.ConversationID == chosen[c.message.ConversationSourceRef] && !used[text.ID] {
				used[text.ID] = true
				copies.phoneOf[c.message.SourceRef] = text
				break
			}
		}
	}
	return copies
}

// split separates the messages to send from the supersessions that retire
// phone copies, with every row such a copy would itself have superseded.
func (copies phoneCopies) split(messages []messagestore.ImportMessage) ([]messagestore.ImportMessage, []messagestore.ImportSupersession) {
	var kept []messagestore.ImportMessage
	var supersessions []messagestore.ImportSupersession
	for _, message := range messages {
		text, isCopy := copies.phoneOf[message.SourceRef]
		if !isCopy || message.Source != SourceMultichat {
			kept = append(kept, message)
			continue
		}
		by := messagestore.MessageKey{Source: phoneSource, SourceRef: text.SourceRef}
		supersessions = append(supersessions, messagestore.ImportSupersession{Superseded: messagestore.MessageKey{Source: message.Source, SourceRef: message.SourceRef}, By: by})
		for _, superseded := range message.Supersedes {
			supersessions = append(supersessions, messagestore.ImportSupersession{Superseded: superseded, By: by})
		}
	}
	return kept, supersessions
}
