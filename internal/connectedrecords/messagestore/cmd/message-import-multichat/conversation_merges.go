package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	messagestore "github.com/kayushkin/message-store"
)

// Google Messages chats reach message-store as up to three conversations: the
// phone archive's thread (source sms, filed by the other people's numbers),
// the mautrix bridge's room and the web bridge's room. They are one chat, so
// they are merged, by ids only:
//
//   - google_conversation_id: rooms whose m.bridge names the same Google
//     conversation (multichat's google_messages_conversation_id) merge into
//     one, the mautrix bridge's room when there is one.
//   - phone_numbers: a direct chat merges into the phone thread with its
//     other person's number, and a group into the phone group whose numbers
//     are exactly its other members' numbers. A number comes from the
//     mautrix bridge's record of its puppet (multichat's
//     GET /google-messages/phone-numbers), else from the people-store person
//     the account belongs to: that person's phone identities and the numbers
//     of their mautrix puppets. A group is matched only through a mautrix
//     room, whose members are the whole group; the web bridge's room lists
//     only those who wrote. The user's own numbers never count.
//   - direct_person: direct chats in different Google conversations with the
//     same people-store person (Google can start a second thread with one
//     person) merge when no phone thread takes them.
//
// A match that is not exactly one thread merges nothing and is counted.

// mergeInputs is what the planner reads.
type mergeInputs struct {
	rooms map[string]room
	// ghostPhones are the mautrix-gmessages puppets' numbers, by user id.
	ghostPhones map[string]string
	// personOfAccount is people-store's person for each Matrix account.
	personOfAccount map[string]string
	// personPhones are people-store's phone identities, by person.
	personPhones map[string][]string
	// phoneThreads are the phone archive's conversations, by source_ref.
	phoneThreads map[string]phoneThread
	selfPerson   string
}

// phoneThread is one sms conversation message-store holds.
type phoneThread struct {
	SourceRef string
	IsGroup   bool
}

// mergeReport counts what the planner found.
type mergeReport struct {
	googleConversations, roomsMerged                 int
	directMatched, directUnmatched, directAmbiguous  int
	groupsMatched, groupsUnmatched, groupsUnreadable int
	directByPerson                                   int
}

func (r mergeReport) String() string {
	return fmt.Sprintf("%d Google conversations; %d rooms merged into another room of the same conversation; direct chats: %d matched a phone thread, %d matched none, %d matched several, %d merged with another thread with the same person; groups: %d matched a phone thread, %d matched none, %d had a member with no number or no mautrix room",
		r.googleConversations, r.roomsMerged, r.directMatched, r.directUnmatched, r.directAmbiguous, r.directByPerson, r.groupsMatched, r.groupsUnmatched, r.groupsUnreadable)
}

func roomKey(roomID string) messagestore.ConversationKey {
	return messagestore.ConversationKey{Source: SourceMatrix, SourceRef: roomID}
}

func isGoogleMessages(platform string) bool {
	return platform == platformGoogleMessagesBridge || platform == platformGoogleMessagesWeb
}

// planConversationMerges decides the merges, in an order the store can apply
// one after another.
func planConversationMerges(in mergeInputs) ([]messagestore.ImportConversationMerge, mergeReport) {
	var merges []messagestore.ImportConversationMerge
	var report mergeReport
	selfPhones := map[string]bool{}
	for _, phone := range in.personPhones[in.selfPerson] {
		selfPhones[phone] = true
	}
	// phonesOf is every number known for an account.
	phonesOf := func(account string) []string {
		if phone, known := in.ghostPhones[account]; known {
			return []string{phone}
		}
		person := in.personOfAccount[account]
		if person == "" {
			return nil
		}
		phones := map[string]bool{}
		for _, phone := range in.personPhones[person] {
			phones[phone] = true
		}
		for other, owner := range in.personOfAccount {
			if owner == person && in.ghostPhones[other] != "" {
				phones[in.ghostPhones[other]] = true
			}
		}
		return sortedKeys(phones)
	}
	groups := map[string][]room{}
	for _, listed := range in.rooms {
		if !isGoogleMessages(listed.Platform) {
			continue
		}
		key := "room:" + listed.RoomID
		if listed.GoogleMessagesConversationID != "" {
			key = "google:" + listed.GoogleMessagesConversationID
		}
		groups[key] = append(groups[key], listed)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	directAnchors := map[string][]string{}
	for _, key := range keys {
		rooms := groups[key]
		// The mautrix room first: it has exact times and every member.
		sort.Slice(rooms, func(i, j int) bool {
			if (rooms[i].Platform == platformGoogleMessagesBridge) != (rooms[j].Platform == platformGoogleMessagesBridge) {
				return rooms[i].Platform == platformGoogleMessagesBridge
			}
			return rooms[i].RoomID < rooms[j].RoomID
		})
		anchor := rooms[0]
		if strings.HasPrefix(key, "google:") {
			report.googleConversations++
		}
		for _, other := range rooms[1:] {
			merges = append(merges, messagestore.ImportConversationMerge{Merged: roomKey(other.RoomID), Into: roomKey(anchor.RoomID), Rule: "google_conversation_id"})
			report.roomsMerged++
		}
		var partners []string
		for _, listed := range rooms {
			if listed.DirectMessagePartnerUserID != "" {
				partners = append(partners, listed.DirectMessagePartnerUserID)
			}
		}
		if len(partners) > 0 {
			phones := map[string]bool{}
			persons := map[string]bool{}
			for _, partner := range partners {
				for _, phone := range phonesOf(partner) {
					phones[phone] = true
				}
				if person := in.personOfAccount[partner]; person != "" {
					persons[person] = true
				}
			}
			var threads []string
			for _, phone := range sortedKeys(phones) {
				if thread, held := in.phoneThreads[phone]; held && !thread.IsGroup && !selfPhones[phone] {
					threads = append(threads, thread.SourceRef)
				}
			}
			switch len(threads) {
			case 1:
				merges = append(merges, messagestore.ImportConversationMerge{Merged: roomKey(anchor.RoomID),
					Into: messagestore.ConversationKey{Source: phoneSource, SourceRef: threads[0]}, Rule: "phone_numbers"})
				report.directMatched++
			case 0:
				report.directUnmatched++
				if len(persons) == 1 {
					for person := range persons {
						directAnchors[person] = append(directAnchors[person], anchor.RoomID)
					}
				}
			default:
				report.directAmbiguous++
			}
			continue
		}
		if anchor.Platform != platformGoogleMessagesBridge {
			report.groupsUnreadable++
			continue
		}
		var numbers []string
		readable := true
		for _, member := range anchor.Members {
			if in.selfPerson != "" && in.personOfAccount[member.UserID] == in.selfPerson {
				continue
			}
			phone := in.ghostPhones[member.UserID]
			if selfPhones[phone] {
				continue
			}
			if phone == "" {
				readable = false
				break
			}
			numbers = append(numbers, phone)
		}
		if !readable || len(numbers) < 2 {
			report.groupsUnreadable++
			continue
		}
		sort.Strings(numbers)
		ref := strings.Join(numbers, ",")
		if thread, held := in.phoneThreads[ref]; held && thread.IsGroup {
			merges = append(merges, messagestore.ImportConversationMerge{Merged: roomKey(anchor.RoomID),
				Into: messagestore.ConversationKey{Source: phoneSource, SourceRef: ref}, Rule: "phone_numbers"})
			report.groupsMatched++
		} else {
			report.groupsUnmatched++
		}
	}
	for _, person := range sortedKeys(boolKeys(directAnchors)) {
		anchors := directAnchors[person]
		sort.Strings(anchors)
		for _, other := range anchors[1:] {
			merges = append(merges, messagestore.ImportConversationMerge{Merged: roomKey(other), Into: roomKey(anchors[0]), Rule: "direct_person"})
			report.directByPerson++
		}
	}
	return merges, report
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func boolKeys[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for key := range m {
		out[key] = true
	}
	return out
}

// readMergeInputs reads what the planner needs beyond multichat's rooms.
func (run runner) readMergeInputs(rooms map[string]room) (mergeInputs, error) {
	in := mergeInputs{rooms: rooms, ghostPhones: map[string]string{}, personOfAccount: map[string]string{}, personPhones: map[string][]string{},
		phoneThreads: map[string]phoneThread{}, selfPerson: run.selfPerson}
	var numbers []struct {
		UserID      string `json:"user_id"`
		PhoneNumber string `json:"phone_number"`
	}
	if err := run.multichat.get("/google-messages/phone-numbers", nil, &numbers); err != nil {
		return in, err
	}
	for _, number := range numbers {
		in.ghostPhones[number.UserID] = number.PhoneNumber
	}
	for _, kind := range []string{"matrix_user", "phone"} {
		for offset := 0; ; offset += 1000 {
			var page []struct {
				PersonID string `json:"person_id"`
				Value    string `json:"value"`
			}
			query := url.Values{"kind": {kind}, "limit": {"1000"}, "offset": {fmt.Sprint(offset)}}
			found, err := getJSON(run.peopleStoreURL+"/identities?"+query.Encode(), nil, &page)
			if err != nil {
				return in, err
			}
			if !found {
				return in, fmt.Errorf("people-store answered GET /identities with 404")
			}
			for _, identity := range page {
				if kind == "phone" {
					in.personPhones[identity.PersonID] = append(in.personPhones[identity.PersonID], identity.Value)
				} else {
					in.personOfAccount[identity.Value] = identity.PersonID
				}
			}
			if len(page) < 1000 {
				break
			}
		}
	}
	const threadLimit = 5000
	var conversations []struct {
		Source    string `json:"source"`
		SourceRef string `json:"source_ref"`
		IsGroup   bool   `json:"is_group"`
	}
	found, err := getJSON(strings.TrimRight(run.storeURL, "/")+"/conversations?"+url.Values{"platform": {"sms"}, "limit": {fmt.Sprint(threadLimit)}}.Encode(), nil, &conversations)
	if err != nil {
		return in, err
	}
	if !found {
		return in, fmt.Errorf("message-store answered GET /conversations with 404")
	}
	if len(conversations) >= threadLimit {
		return in, fmt.Errorf("message-store holds %d or more sms conversations, more than one page; page GET /conversations before merging", threadLimit)
	}
	for _, conversation := range conversations {
		if conversation.Source == phoneSource {
			in.phoneThreads[conversation.SourceRef] = phoneThread{SourceRef: conversation.SourceRef, IsGroup: conversation.IsGroup}
		}
	}
	return in, nil
}
