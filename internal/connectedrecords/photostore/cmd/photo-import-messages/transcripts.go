package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	photostore "github.com/kayushkin/photo-store"
	"github.com/kayushkin/photo-store/internal/importbatch"
	"github.com/kayushkin/photo-store/internal/journalcitation"
	"github.com/kayushkin/photo-store/internal/messagecitation"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// directMessagesFolder is the folder multichat-fetch-discord-history writes
// DMs to, in place of a server id.
const directMessagesFolder = "direct-messages"

// messengerTranscriptTag is the journal-store tag its importer gives
// Messenger transcripts.
const messengerTranscriptTag = "facebook-messenger"

// discordMessageSource is the source message-store's Discord importer files
// a message under, with its Discord message id as the ref.
const discordMessageSource = "discord"

// discordEpochMilliseconds is when Discord's snowflake ids start counting.
const discordEpochMilliseconds = 1420070400000

// facebookMessageFolders are the folders under a Facebook export's messages/
// that hold threads.
var facebookMessageFolders = []string{"inbox", "archived_threads", "filtered_threads", "message_requests", "e2ee_cutover"}

// chatName is a transcript title's chat: "Discord: rexdawn7, 2026-04-05"
// gives "rexdawn7".
func chatName(title string) string {
	_, chat, _ := strings.Cut(title, ": ")
	if comma := strings.LastIndex(chat, ", "); comma >= 0 {
		chat = chat[:comma]
	}
	return chat
}

// importDirectMessages adds the images multichat-fetch-discord-history saved
// from DMs, each citing the message-store message it was sent in (filed under
// its Discord message id) and filed under that conversation's title.
func importDirectMessages(messages messagecitation.Store, in *importbatch.Batch, folder string) error {
	manifest := filepath.Join(folder, "images.jsonl")
	file, err := os.Open(manifest)
	if err != nil {
		return err
	}
	defer file.Close()
	titles := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var line struct {
			MessageID   string `json:"message_id"`
			ContentType string `json:"content_type"`
			Path        string `json:"path"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return fmt.Errorf("%s: %w", manifest, err)
		}
		snowflake, err := strconv.ParseInt(line.MessageID, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: message id %q: %w", manifest, line.MessageID, err)
		}
		found, err := messages.MessageFiledAs(discordMessageSource, line.MessageID)
		if _, notOne := err.(messagecitation.ErrNotOne); notOne {
			in.Report["message-store holds not one message for it ("+err.Error()+")"]++
			continue
		}
		if err != nil {
			return err
		}
		title, known := titles[found.ConversationID]
		if !known {
			conversation, err := messages.ConversationByID(found.ConversationID)
			if err != nil {
				return err
			}
			title = conversation.Title
			titles[found.ConversationID] = title
		}
		original, err := os.ReadFile(filepath.Join(folder, line.Path))
		if err != nil {
			return err
		}
		sentAt := time.UnixMilli(snowflake>>22 + discordEpochMilliseconds)
		in.Add(original, line.ContentType, multichatMachine, found.ID, "discord/direct-messages/"+title, sentAt, photostore.DateFromMessage)
	}
	return scanner.Err()
}

// importFacebookMessages adds the photos in a Facebook export's Messenger
// threads of at most maxMembers people, each cited by the Messenger
// transcript that names its file.
func importFacebookMessages(journal journalcitation.Store, in *importbatch.Batch, messagesFolder string, maxMembers int) error {
	exportRoot := filepath.Dir(filepath.Dir(messagesFolder))
	for _, folder := range facebookMessageFolders {
		files, err := filepath.Glob(filepath.Join(messagesFolder, folder, "*", "message_*.json"))
		if err != nil {
			return err
		}
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var thread struct {
				Participants []json.RawMessage `json:"participants"`
				Messages     []struct {
					TimestampMs int64 `json:"timestamp_ms"`
					Photos      []struct {
						URI string `json:"uri"`
					} `json:"photos"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(raw, &thread); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if len(thread.Participants) > maxMembers {
				continue
			}
			for _, message := range thread.Messages {
				for _, photo := range message.Photos {
					name := path.Base(photo.URI)
					found, err := journal.EntryCiting(messengerTranscriptTag, name)
					if _, notOne := err.(journalcitation.ErrNotOne); notOne {
						in.Report["no one Messenger transcript names its file ("+err.Error()+")"]++
						continue
					}
					if err != nil {
						return err
					}
					original, err := os.ReadFile(filepath.Join(exportRoot, filepath.FromSlash(photo.URI)))
					if os.IsNotExist(err) {
						in.Report["the export lacks the file"]++
						continue
					}
					if err != nil {
						return err
					}
					in.Add(original, mediaTypeOf(name), multichatMachine, found.ID, "messages/facebook/"+chatName(found.Title), time.UnixMilli(message.TimestampMs), photostore.DateFromMessage)
				}
			}
		}
	}
	return nil
}

// mediaTypeOf is a photo's type from its file name; the export records none.
func mediaTypeOf(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
