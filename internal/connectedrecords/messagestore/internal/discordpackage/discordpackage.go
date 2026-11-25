// Package discordpackage reads the Messages folder of a Discord data package
// (the export Discord emails on request): its direct messages and group
// direct messages, never server channels.
//
// The package holds only the account owner's own messages, so a channel is
// one side of a conversation, unless its whole history was also fetched from
// Discord (ReadFullHistory), in which case it has both.
package discordpackage

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The channel types that are conversations with people rather than server
// channels.
const (
	TypeDirectMessage      = "DM"
	TypeGroupDirectMessage = "GROUP_DM"
)

// timestampLayout is how the package writes a message's time, in UTC.
const timestampLayout = "2006-01-02 15:04:05"

// directMessagePrefix starts index.json's name for a DM, before the other
// side's username.
const directMessagePrefix = "Direct Message with "

// Channel is one channel folder: c<id>/channel.json and messages.json, named
// from index.json.
type Channel struct {
	ID         string
	Type       string
	IndexName  string   // index.json's name: "Direct Message with <username>", a group's name, or "None"
	Recipients []string // Discord user ids, the owner's among them
	Messages   []Message
	// FullHistoryFetchedAt is when the channel's whole history, both sides,
	// was fetched from Discord; zero when only the package's side is known.
	FullHistoryFetchedAt time.Time
}

// Attachment is a file sent with a message.
type Attachment struct {
	// ID is Discord's attachment id, when the source names it.
	ID          string
	Name        string
	ContentType string
}

// Message is one message in the channel. A message from the package has no
// author: the owner sent it.
type Message struct {
	ID          string // the Discord snowflake
	Time        time.Time
	Contents    string
	Attachments []Attachment
	AuthorID    string
	AuthorName  string
	// Event names what a message with no text was, such as a call, from
	// Discord's message type.
	Event string
	// IsCall is a call; CallSeconds is how long it lasted, 0 when it never
	// ended or nobody answered.
	IsCall      bool
	CallSeconds int
	// StickerName is the sticker a message with no text was.
	StickerName string
	ReplyToID   string
	Edited      bool
}

type channelFile struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Recipients []string `json:"recipients"`
}

type messageFile struct {
	ID          json.Number `json:"ID"`
	Timestamp   string      `json:"Timestamp"`
	Contents    string      `json:"Contents"`
	Attachments string      `json:"Attachments"`
}

// Read reads every DM and group DM channel in the Messages folder, and how
// many server channels it left out.
func Read(messagesDir string) ([]Channel, int, error) {
	raw, err := os.ReadFile(filepath.Join(messagesDir, "index.json"))
	if err != nil {
		return nil, 0, err
	}
	index := map[string]*string{}
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, 0, fmt.Errorf("index.json: %w", err)
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var channels []Channel
	leftOut := 0
	for _, id := range ids {
		folder := filepath.Join(messagesDir, "c"+id)
		raw, err := os.ReadFile(filepath.Join(folder, "channel.json"))
		if err != nil {
			return nil, 0, err
		}
		var described channelFile
		if err := json.Unmarshal(raw, &described); err != nil {
			return nil, 0, fmt.Errorf("c%s/channel.json: %w", id, err)
		}
		if described.Type != TypeDirectMessage && described.Type != TypeGroupDirectMessage {
			leftOut++
			continue
		}
		raw, err = os.ReadFile(filepath.Join(folder, "messages.json"))
		if err != nil {
			return nil, 0, err
		}
		var sent []messageFile
		if err := json.Unmarshal(raw, &sent); err != nil {
			return nil, 0, fmt.Errorf("c%s/messages.json: %w", id, err)
		}
		channel := Channel{ID: described.ID, Type: described.Type, Recipients: described.Recipients}
		if index[id] != nil {
			channel.IndexName = *index[id]
		}
		for _, message := range sent {
			at, err := time.ParseInLocation(timestampLayout, message.Timestamp, time.UTC)
			if err != nil {
				return nil, 0, fmt.Errorf("c%s message %s: %w", id, message.ID, err)
			}
			converted := Message{ID: message.ID.String(), Time: at, Contents: message.Contents}
			for _, link := range strings.Fields(message.Attachments) {
				converted.Attachments = append(converted.Attachments, attachmentFromLink(link))
			}
			channel.Messages = append(channel.Messages, converted)
		}
		channels = append(channels, channel)
	}
	return channels, leftOut, nil
}

// attachmentFromLink names a package attachment by its CDN link,
// /attachments/<channel>/<attachment id>/<name>. The link itself carries a
// signature that expires, so it is not kept.
func attachmentFromLink(link string) Attachment {
	if !strings.HasPrefix(link, "http") {
		return Attachment{Name: link}
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Path == "" {
		return Attachment{Name: link}
	}
	attachment := Attachment{Name: path.Base(parsed.Path)}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 4 && parts[0] == "attachments" {
		attachment.ID = parts[2]
	}
	return attachment
}

// fetchedMessage is one line of a history fetch: a raw Discord message object.
type fetchedMessage struct {
	ID              string  `json:"id"`
	Type            int     `json:"type"`
	Timestamp       string  `json:"timestamp"`
	EditedTimestamp *string `json:"edited_timestamp"`
	Content         string  `json:"content"`
	Author          struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	} `json:"author"`
	Attachments []struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
	} `json:"attachments"`
	StickerItems []struct {
		Name string `json:"name"`
	} `json:"sticker_items"`
	Call *struct {
		EndedTimestamp *string `json:"ended_timestamp"`
	} `json:"call"`
	MessageReference *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`
}

// callType is Discord's message type for a call.
const callType = 3

// eventNames are Discord's message types that carry no text of their own.
var eventNames = map[int]string{
	1: "added someone to the group", 2: "removed someone from the group", callType: "call",
	4: "renamed the group", 5: "changed the group icon", 6: "pinned a message",
}

// ReadFullHistory reads the channels in a history fetch folder
// (multichat-fetch-discord-history -direct-message): <channel id>.jsonl, used
// only once <channel id>.done marks it complete. It returns each channel's
// messages and when the fetch finished.
func ReadFullHistory(folder string) (map[string][]Message, map[string]time.Time, error) {
	histories := map[string][]Message{}
	fetchedAt := map[string]time.Time{}
	markers, err := filepath.Glob(filepath.Join(folder, "*.done"))
	if err != nil {
		return nil, nil, err
	}
	for _, marker := range markers {
		channelID := strings.TrimSuffix(filepath.Base(marker), ".done")
		raw, err := os.ReadFile(marker)
		if err != nil {
			return nil, nil, err
		}
		finished, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", marker, err)
		}
		raw, err = os.ReadFile(filepath.Join(folder, channelID+".jsonl"))
		if err != nil {
			return nil, nil, err
		}
		var messages []Message
		for number, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var fetched fetchedMessage
			if err := json.Unmarshal([]byte(line), &fetched); err != nil {
				return nil, nil, fmt.Errorf("%s.jsonl line %d: %w", channelID, number+1, err)
			}
			message, err := convertFetched(fetched)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.jsonl line %d: %w", channelID, number+1, err)
			}
			messages = append(messages, message)
		}
		histories[channelID] = messages
		fetchedAt[channelID] = finished
	}
	return histories, fetchedAt, nil
}

func convertFetched(fetched fetchedMessage) (Message, error) {
	at, err := time.Parse(time.RFC3339Nano, fetched.Timestamp)
	if err != nil {
		return Message{}, err
	}
	message := Message{ID: fetched.ID, Time: at.UTC(), Contents: fetched.Content,
		AuthorID: fetched.Author.ID, AuthorName: fetched.Author.GlobalName,
		Edited: fetched.EditedTimestamp != nil && *fetched.EditedTimestamp != ""}
	if message.AuthorName == "" {
		message.AuthorName = fetched.Author.Username
	}
	for _, attachment := range fetched.Attachments {
		message.Attachments = append(message.Attachments, Attachment{ID: attachment.ID, Name: attachment.Filename, ContentType: attachment.ContentType})
	}
	if strings.TrimSpace(fetched.Content) == "" && len(fetched.Attachments) == 0 {
		message.Event = eventNames[fetched.Type]
	}
	if len(fetched.StickerItems) > 0 {
		message.StickerName = fetched.StickerItems[0].Name
	}
	if fetched.MessageReference != nil {
		message.ReplyToID = fetched.MessageReference.MessageID
	}
	if fetched.Type == callType {
		message.IsCall = true
		if fetched.Call != nil && fetched.Call.EndedTimestamp != nil {
			ended, err := time.Parse(time.RFC3339Nano, *fetched.Call.EndedTimestamp)
			if err != nil {
				return Message{}, fmt.Errorf("call end: %w", err)
			}
			message.CallSeconds = int(ended.Sub(at).Seconds())
		}
	}
	return message, nil
}

// MergeFullHistory adds each fetched history to its channel: every fetched
// message, plus any of the package's that the fetch lacks (deleted since).
// It returns how many channels gained a full history.
func MergeFullHistory(channels []Channel, histories map[string][]Message, fetchedAt map[string]time.Time) int {
	merged := 0
	for index := range channels {
		history, found := histories[channels[index].ID]
		if !found {
			continue
		}
		fetchedIDs := map[string]bool{}
		for _, message := range history {
			fetchedIDs[message.ID] = true
		}
		combined := append([]Message(nil), history...)
		for _, message := range channels[index].Messages {
			if !fetchedIDs[message.ID] {
				combined = append(combined, message)
			}
		}
		channels[index].Messages = combined
		channels[index].FullHistoryFetchedAt = fetchedAt[channels[index].ID]
		merged++
	}
	return merged
}

// DirectMessageUsername is the other side's username from index.json, or ""
// when the channel is not a DM or the index does not name it.
func DirectMessageUsername(channel Channel) string {
	if channel.Type != TypeDirectMessage {
		return ""
	}
	username, found := strings.CutPrefix(channel.IndexName, directMessagePrefix)
	if !found {
		return ""
	}
	return username
}

// AuthorNames maps each author id to the latest name the fetched messages
// give it (a fetch runs newest first, so the first seen is the latest), with
// names overriding.
func AuthorNames(channels []Channel, names map[string]string) map[string]string {
	all := map[string]string{}
	for _, channel := range channels {
		for _, message := range channel.Messages {
			if _, known := all[message.AuthorID]; message.AuthorID != "" && message.AuthorName != "" && !known {
				all[message.AuthorID] = message.AuthorName
			}
		}
	}
	for id, name := range names {
		all[id] = name
	}
	return all
}

// OtherMembers are the channel's recipients other than the owner, then anyone
// else who wrote in it (someone who has since left a group), each once.
func OtherMembers(channel Channel, selfID string) []string {
	var members []string
	seen := map[string]bool{selfID: true}
	for _, recipient := range channel.Recipients {
		if !seen[recipient] {
			seen[recipient] = true
			members = append(members, recipient)
		}
	}
	for _, message := range channel.Messages {
		if message.AuthorID != "" && !seen[message.AuthorID] {
			seen[message.AuthorID] = true
			members = append(members, message.AuthorID)
		}
	}
	return members
}

// Title names a channel as the journal transcripts did, without their
// "Discord:" prefix and day.
func Title(channel Channel, selfID string, names map[string]string) string {
	if username := DirectMessageUsername(channel); username != "" {
		return username
	}
	if channel.Type == TypeGroupDirectMessage && channel.IndexName != "" && channel.IndexName != "None" {
		return "group " + channel.IndexName
	}
	var named []string
	for _, recipient := range channel.Recipients {
		if recipient != selfID && names[recipient] != "" {
			named = append(named, names[recipient])
		}
	}
	if channel.Type == TypeGroupDirectMessage {
		if len(named) == 0 {
			return "group " + channel.ID
		}
		return "group with " + strings.Join(named, ", ")
	}
	if len(named) == 1 {
		return named[0]
	}
	return "DM " + channel.ID
}
