// Command message-import-discord imports the direct messages and group direct
// messages of a Discord data package into message-store, with the whole
// history of any channel a -full-history fetch holds: one conversation per
// channel, one message per Discord message, and one conversation day per
// channel per day, naming the journal-store transcript (source
// discord-package, source_ref "<channel id>/<day>") that held it.
//
//	message-import-discord -messages <package>/Messages -self-id 100000000000000001 -self-name 'Me Self' \
//	    -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago' \
//	    -people people.json -names names.json -full-history <fetch folder> -dry-run
//
// A message is keyed by source discord and its snowflake, the namespace every
// Discord importer shares, so the same message read from the package, a fetch
// or a live bridge is kept once. A picture the fetch saved (images.jsonl) is
// linked to its photo-store photo by the file's sha256.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/discordpackage"
	"github.com/kayushkin/message-store/internal/importclient"
	"github.com/kayushkin/message-store/internal/photolookup"
	"github.com/kayushkin/message-store/internal/residencezones"
)

const importerName = "message-import-discord"

// Platform and Source name the conversation and message id namespace.
const (
	Platform = "discord"
	Source   = "discord"
)

func main() {
	messagesDir := flag.String("messages", "", "the package's Messages folder, holding index.json and c<channel id>/ folders")
	selfID := flag.String("self-id", "", "the owner's Discord user id")
	selfName := flag.String("self-name", "", "the owner's name to show")
	timeZones := flag.String("time-zones", "", "where the owner lived: first zone, then day=zone for each move (Europe/Lisbon,2021-07-11=America/Chicago)")
	peoplePath := flag.String("people", "", "JSON object mapping Discord user ids or usernames to people-store ids")
	namesPath := flag.String("names", "", "JSON object mapping Discord user ids to names to show")
	fullHistory := flag.String("full-history", "", "a folder of whole-conversation fetches (multichat-fetch-discord-history -direct-message): channels found there get both sides, and its images.jsonl pictures")
	journalSource := flag.String("journal-source", "discord-package", "the journal-store source the transcripts were filed under")
	dryRun := flag.Bool("dry-run", false, "read everything and count it; send nothing")
	flag.Parse()
	if *messagesDir == "" || *selfID == "" || *selfName == "" || *timeZones == "" {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
	options := options{selfID: *selfID, selfName: *selfName, journalSource: *journalSource}
	if err := run(*messagesDir, *timeZones, *peoplePath, *namesPath, *fullHistory, options,
		settings.String(messagestore.SettingStoreURL), settings.String(messagestore.SettingPhotoStoreURL), *dryRun, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
}

type options struct {
	selfID, selfName, journalSource string
	people, names                   map[string]string
	zones                           residencezones.Zones
	// photoIDs maps a Discord attachment id to its photo-store photo.
	photoIDs map[string]string
}

func run(messagesDir, timeZones, peoplePath, namesPath, fullHistory string, opts options, storeURL, photoStoreURL string, dryRun bool, out io.Writer) error {
	var err error
	if opts.zones, err = residencezones.Parse(timeZones); err != nil {
		return fmt.Errorf("-time-zones: %w", err)
	}
	if opts.people, err = importclient.ReadStringMap(peoplePath); err != nil {
		return fmt.Errorf("-people: %w", err)
	}
	if opts.names, err = importclient.ReadStringMap(namesPath); err != nil {
		return fmt.Errorf("-names: %w", err)
	}
	channels, leftOut, err := discordpackage.Read(messagesDir)
	if err != nil {
		return err
	}
	withFullHistory := 0
	opts.photoIDs = map[string]string{}
	if fullHistory != "" {
		histories, fetchedAt, err := discordpackage.ReadFullHistory(fullHistory)
		if err != nil {
			return fmt.Errorf("-full-history: %w", err)
		}
		withFullHistory = discordpackage.MergeFullHistory(channels, histories, fetchedAt)
		lookup := photolookup.New(photoStoreURL)
		if opts.photoIDs, err = savedPictures(fullHistory, lookup); err != nil {
			return fmt.Errorf("-full-history: %w", err)
		}
		fmt.Fprintln(out, lookup.Report())
	}
	fmt.Fprintf(out, "read %d DM and group DM channels (%d with both sides fetched); left out %d server channels\n", len(channels), withFullHistory, leftOut)
	client := importclient.New(storeURL, dryRun)
	if err := client.Begin(importerName, "a Discord data package's direct messages, with fetched whole histories"); err != nil {
		return err
	}
	if err := send(client, channels, opts); err != nil {
		return err
	}
	if err := client.Finish(); err != nil {
		return err
	}
	fmt.Fprintln(out, client.Report())
	return nil
}

// savedPictures reads the fetch's images.jsonl, the pictures it saved, and
// finds each in photo-store by the file's hash. A folder with no manifest
// saved none.
func savedPictures(folder string, lookup *photolookup.Lookup) (map[string]string, error) {
	photoIDs := map[string]string{}
	manifest := filepath.Join(folder, "images.jsonl")
	file, err := os.Open(manifest)
	if os.IsNotExist(err) {
		return photoIDs, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var line struct {
			AttachmentID string `json:"attachment_id"`
			Path         string `json:"path"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s: %w", manifest, err)
		}
		original, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(line.Path)))
		if err != nil {
			return nil, err
		}
		photoID, err := lookup.PhotoID(original)
		if err != nil {
			return nil, err
		}
		if photoID != "" {
			photoIDs[line.AttachmentID] = photoID
		}
	}
	return photoIDs, scanner.Err()
}

func send(sink importclient.Sink, channels []discordpackage.Channel, opts options) error {
	names := discordpackage.AuthorNames(channels, opts.names)
	for _, channel := range channels {
		if len(channel.Messages) == 0 {
			continue
		}
		username := discordpackage.DirectMessageUsername(channel)
		conversation := messagestore.ImportConversation{Platform: Platform, Source: Source, SourceRef: channel.ID,
			Title: discordpackage.Title(channel, opts.selfID, names), IsGroup: channel.Type == discordpackage.TypeGroupDirectMessage,
			Participants: []messagestore.Participant{{Handle: opts.selfID, DisplayName: opts.selfName, PersonID: opts.people[opts.selfID], IsSelf: true}}}
		for _, member := range discordpackage.OtherMembers(channel, opts.selfID) {
			participant := messagestore.Participant{Handle: member, DisplayName: names[member], PersonID: opts.people[member]}
			if channel.Type == discordpackage.TypeDirectMessage && username != "" {
				if participant.DisplayName == "" {
					participant.DisplayName = username
				}
				if participant.PersonID == "" {
					participant.PersonID = opts.people[username]
				}
			}
			conversation.Participants = append(conversation.Participants, participant)
		}
		if err := sink.AddConversation(conversation); err != nil {
			return err
		}
		messages := append([]discordpackage.Message(nil), channel.Messages...)
		sort.SliceStable(messages, func(i, j int) bool { return messages[i].Time.Before(messages[j].Time) })
		dayZones := map[string]string{}
		var days []string
		for _, message := range messages {
			if err := sink.AddMessage(importMessage(channel.ID, message, opts)); err != nil {
				return err
			}
			// The day and zone the journal transcript was filed under.
			location := opts.zones.At(message.Time)
			day := message.Time.In(location).Format(time.DateOnly)
			if _, seen := dayZones[day]; !seen {
				dayZones[day] = location.String()
				days = append(days, day)
			}
		}
		sort.Strings(days)
		for _, day := range days {
			if err := sink.AddConversationDay(messagestore.ImportConversationDay{ConversationSource: Source, ConversationSourceRef: channel.ID,
				Day: day, TimeZone: dayZones[day], JournalSource: opts.journalSource, JournalSourceRef: channel.ID + "/" + day}); err != nil {
				return err
			}
		}
	}
	return nil
}

func importMessage(channelID string, message discordpackage.Message, opts options) messagestore.ImportMessage {
	sender := message.AuthorID
	if sender == "" {
		// The package holds only the owner's messages, and names no author.
		sender = opts.selfID
	}
	imported := messagestore.ImportMessage{BodySource: "platform", ConversationSource: Source, ConversationSourceRef: channelID, SentAt: message.Time.UnixMilli(),
		SenderHandle: sender, Kind: "text", Body: message.Contents, ReplyToSourceRef: message.ReplyToID, Edited: message.Edited,
		Source: Source, SourceRef: message.ID}
	for _, attachment := range message.Attachments {
		imported.Attachments = append(imported.Attachments, messagestore.Attachment{PhotoID: opts.photoIDs[attachment.ID],
			MimeType: attachment.ContentType, Name: attachment.Name})
	}
	empty := strings.TrimSpace(message.Contents) == ""
	switch {
	case message.IsCall:
		imported.Kind = "call"
		imported.CallSeconds = message.CallSeconds
	case message.Event != "":
		imported.Kind, imported.Body = "system", message.Event
	case empty && message.StickerName != "":
		imported.Kind, imported.Body = "sticker", message.StickerName
	case empty && len(message.Attachments) > 0:
		imported.Kind = "media"
	}
	imported.SourceHash = importclient.MessageHash(imported)
	return imported
}
