// Command message-import-facebook-messenger imports the Messenger chats of a
// Facebook data export (JSON format) into message-store: one conversation per
// thread of at most eight people, one message per message, both sides, and
// one conversation day per thread per day, naming the journal-store
// transcript (source facebook-messenger, source_ref "<folder>/<thread>/<day>")
// that held it.
//
//	message-import-facebook-messenger -messages <export>/your_facebook_activity/messages \
//	    -self-name 'Me Self' -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago' \
//	    -people people.json -dry-run
//
// The export names people only by display name, so a participant's handle is
// that name, and -people maps names to people-store ids. A photo or GIF the
// export holds is linked to its photo-store photo by the file's sha256.
package main

import (
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
	"github.com/kayushkin/message-store/internal/facebookmessages"
	"github.com/kayushkin/message-store/internal/importclient"
	"github.com/kayushkin/message-store/internal/photolookup"
	"github.com/kayushkin/message-store/internal/residencezones"
)

const importerName = "message-import-facebook-messenger"

// Platform, Source and JournalSource name the conversation and message id
// namespace and the journal-store source the transcripts were filed under.
const (
	Platform      = "facebook-messenger"
	Source        = "facebook-messenger"
	JournalSource = "facebook-messenger"
)

func main() {
	messagesDir := flag.String("messages", "", "the export's your_facebook_activity/messages folder")
	selfName := flag.String("self-name", "", "the user's own name as the export writes it")
	timeZones := flag.String("time-zones", "", "where the user lived: first zone, then day=zone for each move (Europe/Lisbon,2021-07-11=America/Chicago)")
	peoplePath := flag.String("people", "", "JSON object mapping names as the export writes them to people-store ids")
	dryRun := flag.Bool("dry-run", false, "read everything and count it; send nothing")
	flag.Parse()
	if *messagesDir == "" || *selfName == "" || *timeZones == "" {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
	if err := run(*messagesDir, *selfName, *timeZones, *peoplePath, settings.String(messagestore.SettingStoreURL),
		settings.String(messagestore.SettingPhotoStoreURL), *dryRun, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
}

type options struct {
	selfName string
	people   map[string]string
	zones    residencezones.Zones
	// photoID finds a sent file's photo-store photo; "" when there is none.
	photoID func(facebookmessages.File) (string, error)
}

func run(messagesDir, selfName, timeZones, peoplePath, storeURL, photoStoreURL string, dryRun bool, out io.Writer) error {
	opts := options{selfName: selfName}
	var err error
	if opts.zones, err = residencezones.Parse(timeZones); err != nil {
		return fmt.Errorf("-time-zones: %w", err)
	}
	if opts.people, err = importclient.ReadStringMap(peoplePath); err != nil {
		return fmt.Errorf("-people: %w", err)
	}
	threads, skipped, err := facebookmessages.Read(messagesDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read %d threads; left out %d threads of more than %d people (%d messages)\n",
		len(threads), skipped.TooManyParticipants, facebookmessages.MaximumParticipants, skipped.Messages)
	// A file's uri is relative to the export's root, two folders up.
	exportRoot := filepath.Dir(filepath.Dir(filepath.Clean(messagesDir)))
	lookup := photolookup.New(photoStoreURL)
	missingFiles := 0
	opts.photoID = func(file facebookmessages.File) (string, error) {
		if file.Kind != "photo" && file.Kind != "gif" {
			return "", nil
		}
		original, err := os.ReadFile(filepath.Join(exportRoot, filepath.FromSlash(file.URI)))
		if os.IsNotExist(err) {
			missingFiles++
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return lookup.PhotoID(original)
	}
	client := importclient.New(storeURL, dryRun)
	if err := client.Begin(importerName, "a Facebook export's Messenger threads of at most eight people"); err != nil {
		return err
	}
	if err := send(client, threads, opts); err != nil {
		return err
	}
	if err := client.Finish(); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s; %d files the export lacks\n", lookup.Report(), missingFiles)
	fmt.Fprintln(out, client.Report())
	return nil
}

func send(sink importclient.Sink, threads []facebookmessages.Thread, opts options) error {
	for _, thread := range threads {
		conversation := messagestore.ImportConversation{Platform: Platform, Source: Source, SourceRef: thread.Ref(),
			Title: facebookmessages.Title(thread, opts.selfName), IsGroup: thread.ParticipantCount > 2}
		listed := map[string]bool{}
		addParticipant := func(name string) {
			if name == "" || listed[name] {
				return
			}
			listed[name] = true
			conversation.Participants = append(conversation.Participants, messagestore.Participant{Handle: name, DisplayName: name,
				PersonID: opts.people[name], IsSelf: name == opts.selfName})
		}
		for _, name := range thread.Participants {
			addParticipant(name)
		}
		for _, message := range thread.Messages {
			// Someone who wrote and has since left is still a participant.
			addParticipant(message.Sender)
		}
		if err := sink.AddConversation(conversation); err != nil {
			return err
		}
		messages := append([]facebookmessages.Message(nil), thread.Messages...)
		sort.SliceStable(messages, func(i, j int) bool { return messages[i].Time.Before(messages[j].Time) })
		dayZones := map[string]string{}
		var days []string
		for _, message := range messages {
			imported, err := importMessage(thread, message, opts)
			if err != nil {
				return err
			}
			if err := sink.AddMessage(imported); err != nil {
				return err
			}
			location := opts.zones.At(message.Time)
			day := message.Time.In(location).Format(time.DateOnly)
			if _, seen := dayZones[day]; !seen {
				dayZones[day] = location.String()
				days = append(days, day)
			}
		}
		sort.Strings(days)
		for _, day := range days {
			if err := sink.AddConversationDay(messagestore.ImportConversationDay{ConversationSource: Source, ConversationSourceRef: thread.Ref(),
				Day: day, TimeZone: dayZones[day], JournalSource: JournalSource, JournalSourceRef: thread.Ref() + "/" + day}); err != nil {
				return err
			}
		}
	}
	return nil
}

func importMessage(thread facebookmessages.Thread, message facebookmessages.Message, opts options) (messagestore.ImportMessage, error) {
	imported := messagestore.ImportMessage{BodySource: "platform", ConversationSource: Source, ConversationSourceRef: thread.Ref(), SentAt: message.Time.UnixMilli(),
		SenderHandle: message.Sender, Kind: "text", Body: message.Content, DeletedOnPlatform: message.IsUnsent,
		Source: Source, SourceRef: message.Ref}
	if message.ShareLink != "" && !strings.Contains(imported.Body, message.ShareLink) {
		// The link a message shared is part of what it said.
		imported.Body = strings.TrimSpace(imported.Body + "\n" + message.ShareLink)
	}
	for _, file := range message.Files {
		photoID, err := opts.photoID(file)
		if err != nil {
			return messagestore.ImportMessage{}, err
		}
		imported.Attachments = append(imported.Attachments, messagestore.Attachment{PhotoID: photoID, MimeType: mimeType(file), Name: file.Name()})
	}
	empty := strings.TrimSpace(imported.Body) == ""
	switch {
	case message.IsCall:
		imported.Kind = "call"
		imported.CallSeconds = message.CallSeconds
	case empty && message.Sticker:
		imported.Kind = "sticker"
	case empty && len(message.Files) > 0:
		imported.Kind = "media"
	}
	imported.SourceHash = importclient.MessageHash(imported)
	return imported, nil
}

// mimeType is a file's type from its name; the export records none. Only
// the kinds whose names say for certain are given one.
func mimeType(file facebookmessages.File) string {
	switch strings.ToLower(filepath.Ext(file.Name())) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	}
	return ""
}
