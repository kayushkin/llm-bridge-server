package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/importclient"
)

// MaximumMembers is the largest conversation, the user included, imported
// without being named by a flag.
const MaximumMembers = 8

// Id namespaces. A row that carries a Discord message id is filed under
// discord, as the Discord data package files it, so the two collapse into one
// row; every other row is filed by multichat's own row ref. Until multichat
// gave live Discord rows their Discord id (2026-10-30) they were filed by
// ref, so a live Discord message names its rows' refs in supersedes. Every Discord
// conversation is its channel, and every other conversation its Matrix room.
const (
	SourceDiscord   = "discord"
	SourceMultichat = "multichat"
	SourceMatrix    = "matrix"
)

// platforms maps multichat's platform names (GET /conversations, or the log
// row's own when the room is not listed) to message-store's.
var platforms = map[string]string{
	"discord":      "discord",
	"gmessages":    "sms",
	"gmessagesweb": "sms",
	"facebook":     "facebook-messenger",
	"instagram":    "instagram",
	"whatsapp":     "whatsapp",
	"telegram":     "telegram",
	"signal":       "signal",
	"slack":        "slack",
	"googlechat":   "google-chat",
}

// loggedMessage is one row of multichat's GET /message-log.
type loggedMessage struct {
	Ref               string     `json:"ref"`
	EventID           string     `json:"event_id"`
	Source            string     `json:"source"`
	DiscordMessageID  string     `json:"discord_message_id"`
	RoomID            string     `json:"room_id"`
	RoomName          string     `json:"room_name"`
	Platform          string     `json:"platform"`
	NetworkID         string     `json:"network_id"`
	NetworkName       string     `json:"network_name"`
	ChannelID         string     `json:"channel_id"`
	SenderUserID      string     `json:"sender_user_id"`
	SenderDisplayName string     `json:"sender_display_name"`
	MessageType       string     `json:"message_type"`
	Body              string     `json:"body"`
	MediaMimetype     string     `json:"media_mimetype"`
	ReplyToEventID    string     `json:"reply_to_event_id"`
	ContentJSON       string     `json:"content_json"`
	ReplacesEventID   string     `json:"replaces_event_id"`
	SentAt            time.Time  `json:"sent_at"`
	RedactedAt        *time.Time `json:"redacted_at"`
}

// logPage is GET /message-log's answer.
type logPage struct {
	Messages []loggedMessage            `json:"messages"`
	Edits    map[string][]loggedMessage `json:"edits"`
}

// room is one row of multichat's GET /conversations.
type room struct {
	RoomID      string `json:"room_id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	MemberCount int    `json:"member_count"`
	Members     []struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
	} `json:"members"`
	// DirectMessagePartnerUserID is the other person in a room its bridge
	// marks one-to-one.
	DirectMessagePartnerUserID string `json:"direct_message_partner_user_id"`
	// GoogleMessagesConversationID is Google's id for a Google Messages
	// room's conversation, which both bridges' rooms for it share.
	GoogleMessagesConversationID string `json:"google_messages_conversation_id"`
}

// discordArchiveContent is the part of a Discord message object this command
// reads from an archive row's content_json.
type discordArchiveContent struct {
	Type            int     `json:"type"`
	Content         string  `json:"content"`
	EditedTimestamp *string `json:"edited_timestamp"`
	Attachments     []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
	} `json:"attachments"`
	StickerItems []struct {
		Name string `json:"name"`
	} `json:"sticker_items"`
	MessageReference *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`
}

// matrixMediaContent is what a live media row's content says beyond the
// row's own columns.
type matrixMediaContent struct {
	Filename string `json:"filename"`
}

// Discord message types that carry what a person wrote: a message and a
// reply. Every other type is a line Discord wrote.
const (
	discordDefaultMessage = 0
	discordReply          = 19
)

var discordPuppet = regexp.MustCompile(`^@discord_(\d+):`)

// selection says which rooms the user chose beyond the default.
type selection struct {
	rooms          map[string]bool
	discordServers map[string]bool
}

// skipReason is why a row is left out, "" when it is imported.
func (s selection) skipReason(row loggedMessage, rooms map[string]room) string {
	switch {
	case s.rooms[row.RoomID]:
		return ""
	case row.Platform == "discord" && row.NetworkID != "":
		if s.discordServers[row.NetworkID] {
			return ""
		}
		return "a Discord server not named by -discord-server"
	}
	listed, known := rooms[row.RoomID]
	switch {
	case !known:
		return "a room multichat no longer lists, so its size is unknown"
	case listed.MemberCount > MaximumMembers:
		return fmt.Sprintf("a group of more than %d", MaximumMembers)
	}
	return ""
}

// converter turns log rows into message-store records.
type converter struct {
	rooms map[string]room
	// people are people-store's ids by handle, as resolve found them.
	people map[string]string
	// selfPerson is the user's people-store id.
	selfPerson string
	// selfUserIDs are the Matrix users multichat marks as the user's own.
	selfUserIDs map[string]bool
	// photos are photo-store ids by message-log ref, in photo-store's order.
	photos map[string][]photoRef
	// events are the rows read so far by Matrix event id, so a reply finds
	// its target's source_ref.
	events map[string]loggedMessage
	// edits are the edits read, by the event id they edit, oldest first.
	edits map[string][]loggedMessage
	// supersededByRow are, by a row's ref, the refs of other rows that are
	// the same message and are filed as it (google_messages_twins.go).
	supersededByRow map[string][]string
}

// photoRef is one photo-store photo of a message.
type photoRef struct {
	ID        string
	MediaType string
}

// platformOf is message-store's platform for a row.
func (c converter) platformOf(row loggedMessage) (string, error) {
	name := row.Platform
	if listed, known := c.rooms[row.RoomID]; known && listed.Platform != "" {
		name = listed.Platform
	}
	platform, known := platforms[name]
	if !known {
		return "", fmt.Errorf("%s: multichat platform %q has no message-store platform", row.Ref, name)
	}
	return platform, nil
}

// handleOf is a sender's handle: the Discord user id on Discord, where the
// data package names people by it, else the Matrix user id.
func handleOf(platform, matrixUserID string) string {
	if platform == "discord" {
		if match := discordPuppet.FindStringSubmatch(matrixUserID); match != nil {
			return match[1]
		}
	}
	return matrixUserID
}

// conversationKey is the conversation a row belongs to.
func conversationKey(platform string, row loggedMessage) (source, sourceRef string, err error) {
	if platform == "discord" {
		if row.ChannelID == "" {
			return "", "", fmt.Errorf("%s: a Discord row with no channel id", row.Ref)
		}
		return SourceDiscord, row.ChannelID, nil
	}
	return SourceMatrix, row.RoomID, nil
}

// messageKey is a row's own id: the Discord snowflake when the row has one.
func messageKey(row loggedMessage) (source, sourceRef string) {
	if row.DiscordMessageID != "" {
		return SourceDiscord, row.DiscordMessageID
	}
	return SourceMultichat, row.Ref
}

// isSelf says whether a row's sender is the user: people-store gives the
// handle to the user's person, or multichat marked it as the user's own.
func (c converter) isSelf(row loggedMessage) bool {
	return (c.selfPerson != "" && c.people[row.SenderUserID] == c.selfPerson) || c.selfUserIDs[row.SenderUserID]
}

// message converts one row. It needs every edit and reply target read first.
func (c converter) message(row loggedMessage) (messagestore.ImportMessage, error) {
	platform, err := c.platformOf(row)
	if err != nil {
		return messagestore.ImportMessage{}, err
	}
	conversationSource, conversationRef, err := conversationKey(platform, row)
	if err != nil {
		return messagestore.ImportMessage{}, err
	}
	source, sourceRef := messageKey(row)
	message := messagestore.ImportMessage{ConversationSource: conversationSource, ConversationSourceRef: conversationRef,
		SentAt: row.SentAt.UnixMilli(), SenderHandle: handleOf(platform, row.SenderUserID), Source: source, SourceRef: sourceRef,
		DeletedOnPlatform: row.RedactedAt != nil, Attachments: []messagestore.Attachment{}}
	if row.Source == "discord-archive" {
		var content discordArchiveContent
		if err := json.Unmarshal([]byte(row.ContentJSON), &content); err != nil {
			return messagestore.ImportMessage{}, fmt.Errorf("%s: content_json: %w", row.Ref, err)
		}
		message.Body, message.BodySource = content.Content, "platform"
		message.Edited = content.EditedTimestamp != nil
		for _, attachment := range content.Attachments {
			message.Attachments = append(message.Attachments, messagestore.Attachment{MimeType: attachment.ContentType, Name: attachment.Filename})
		}
		if content.MessageReference != nil {
			message.ReplyToSourceRef = content.MessageReference.MessageID
		}
		switch {
		case content.Type != discordDefaultMessage && content.Type != discordReply:
			message.Kind = "system"
		case message.Body == "" && len(content.StickerItems) > 0:
			message.Kind, message.Body = "sticker", content.StickerItems[0].Name
		case message.Body == "" && len(message.Attachments) > 0:
			message.Kind = "media"
		default:
			message.Kind = "text"
		}
	} else {
		latest := row
		if edits := c.edits[row.EventID]; len(edits) > 0 {
			latest = edits[len(edits)-1]
			message.Edited = true
		}
		// The bridge's rendering: Discord's tokens already turned into names and times.
		message.Body, message.BodySource = latest.Body, "bridge"
		if target, known := c.events[row.ReplyToEventID]; known && row.ReplyToEventID != "" {
			_, message.ReplyToSourceRef = messageKey(target)
		}
		switch latest.MessageType {
		case "m.image", "m.video", "m.file", "m.audio":
			var media matrixMediaContent
			if err := json.Unmarshal([]byte(latest.ContentJSON), &media); err != nil {
				return messagestore.ImportMessage{}, fmt.Errorf("%s: content_json: %w", row.Ref, err)
			}
			// A body that is not the file's name is a caption.
			name := latest.Body
			if media.Filename != "" && media.Filename != latest.Body {
				name = media.Filename
			} else {
				message.Body = ""
			}
			message.Attachments = append(message.Attachments, messagestore.Attachment{MimeType: latest.MediaMimetype, Name: name})
			message.Kind = "media"
			if message.Body != "" {
				message.Kind = "text"
			}
		case "m.sticker":
			message.Kind = "sticker"
		default:
			message.Kind = "text"
		}
	}
	c.attachPhotos(&message, row.Ref)
	message.SourceHash = importclient.MessageHash(message)
	return message, nil
}

// messages converts the imported rows, newest first. The live rows of one
// Discord message (its text and each attachment) become one message.
func (c converter) messages(rows []loggedMessage) ([]messagestore.ImportMessage, error) {
	parts := map[string][]loggedMessage{}
	for _, row := range rows {
		if row.Source == "matrix" && row.DiscordMessageID != "" {
			parts[row.DiscordMessageID] = append(parts[row.DiscordMessageID], row)
		}
	}
	var out []messagestore.ImportMessage
	done := map[string]bool{}
	for _, row := range rows {
		group := []loggedMessage{row}
		if row.Source == "matrix" && row.DiscordMessageID != "" {
			if done[row.DiscordMessageID] {
				continue
			}
			done[row.DiscordMessageID] = true
			group = parts[row.DiscordMessageID]
		}
		message, err := c.joinParts(group)
		if err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, nil
}

// joinParts converts the rows of one message. Rows of a live Discord message
// share its id: the text comes from the part that has it, attachments from
// every part in the order multichat logged them, and the message counts as
// deleted only when every part is. Each live row's multichat ref is
// superseded.
func (c converter) joinParts(rows []loggedMessage) (messagestore.ImportMessage, error) {
	sorted := append([]loggedMessage(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ref < sorted[j].Ref })
	var converted []messagestore.ImportMessage
	for _, row := range sorted {
		message, err := c.message(row)
		if err != nil {
			return messagestore.ImportMessage{}, err
		}
		converted = append(converted, message)
	}
	joined := converted[0]
	for _, part := range converted {
		if part.Body != "" && part.Kind == "text" {
			joined = part
			break
		}
	}
	if len(converted) > 1 {
		joined.Attachments = []messagestore.Attachment{}
		allDeleted := true
		for _, part := range converted {
			joined.Attachments = append(joined.Attachments, part.Attachments...)
			joined.Edited = joined.Edited || part.Edited
			allDeleted = allDeleted && part.DeletedOnPlatform
			if part.SentAt < joined.SentAt {
				joined.SentAt = part.SentAt
			}
			if joined.ReplyToSourceRef == "" {
				joined.ReplyToSourceRef = part.ReplyToSourceRef
			}
		}
		joined.DeletedOnPlatform = allDeleted
		if joined.Body == "" && len(joined.Attachments) > 0 && joined.Kind != "sticker" {
			joined.Kind = "media"
		}
	}
	for _, row := range sorted {
		if row.Source == "matrix" && row.DiscordMessageID != "" {
			joined.Supersedes = append(joined.Supersedes, messagestore.MessageKey{Source: SourceMultichat, SourceRef: row.Ref})
		}
	}
	for _, row := range sorted {
		for _, ref := range c.supersededByRow[row.Ref] {
			joined.Supersedes = append(joined.Supersedes, messagestore.MessageKey{Source: SourceMultichat, SourceRef: ref})
		}
	}
	joined.SourceHash = ""
	joined.SourceHash = importclient.MessageHash(joined)
	return joined, nil
}

// attachPhotos gives each picture or video the photo-store id photo-store
// filed under the row's ref, in order; a photo with no attachment to carry it
// becomes one of its own.
func (c converter) attachPhotos(message *messagestore.ImportMessage, ref string) {
	photos := c.photos[ref]
	next := 0
	for index := range message.Attachments {
		mime := message.Attachments[index].MimeType
		if next < len(photos) && (strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/")) {
			message.Attachments[index].PhotoID = photos[next].ID
			next++
		}
	}
	for ; next < len(photos); next++ {
		message.Attachments = append(message.Attachments, messagestore.Attachment{PhotoID: photos[next].ID, MimeType: photos[next].MediaType})
	}
}

// conversations gathers each imported row's conversation with everyone seen
// in it and the members multichat lists.
func (c converter) conversations(rows []loggedMessage) ([]messagestore.ImportConversation, error) {
	type gathered struct {
		conversation messagestore.ImportConversation
		handles      map[string]int
	}
	byKey := map[string]*gathered{}
	var order []string
	add := func(g *gathered, participant messagestore.Participant) {
		if index, seen := g.handles[participant.Handle]; seen {
			existing := &g.conversation.Participants[index]
			if participant.DisplayName != "" {
				existing.DisplayName = participant.DisplayName
			}
			existing.IsSelf = existing.IsSelf || participant.IsSelf
			return
		}
		g.handles[participant.Handle] = len(g.conversation.Participants)
		g.conversation.Participants = append(g.conversation.Participants, participant)
	}
	// Rows come newest first; the oldest-first walk leaves each handle with
	// its newest display name.
	sorted := append([]loggedMessage(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SentAt.Before(sorted[j].SentAt) })
	for _, row := range sorted {
		platform, err := c.platformOf(row)
		if err != nil {
			return nil, err
		}
		source, sourceRef, err := conversationKey(platform, row)
		if err != nil {
			return nil, err
		}
		key := source + "\x00" + sourceRef
		g := byKey[key]
		if g == nil {
			listed, isListed := c.rooms[row.RoomID]
			title := row.RoomName
			isGroup := listed.MemberCount > 2
			// A Google Messages room is one-to-one when its bridge says so (a
			// web bridge room lists only those who wrote), and multichat's
			// name for it lists the other people where Google's names the user.
			if isListed && isGoogleMessages(listed.Platform) {
				isGroup = listed.DirectMessagePartnerUserID == ""
				title = listed.Name
			}
			if platform == "discord" && row.NetworkID != "" {
				title = strings.TrimSpace(row.NetworkName + " " + row.RoomName)
				isGroup = true
			}
			g = &gathered{conversation: messagestore.ImportConversation{Platform: platform, Source: source, SourceRef: sourceRef, Title: title, IsGroup: isGroup},
				handles: map[string]int{}}
			byKey[key] = g
			order = append(order, key)
			for _, member := range listed.Members {
				add(g, messagestore.Participant{Handle: handleOf(platform, member.UserID), DisplayName: member.DisplayName, PersonID: c.people[member.UserID]})
			}
		}
		add(g, messagestore.Participant{Handle: handleOf(platform, row.SenderUserID), DisplayName: row.SenderDisplayName,
			PersonID: c.people[row.SenderUserID], IsSelf: c.isSelf(row)})
	}
	conversations := make([]messagestore.ImportConversation, 0, len(order))
	for _, key := range order {
		conversations = append(conversations, byKey[key].conversation)
	}
	return conversations, nil
}
