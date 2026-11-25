// Command message-import-skype imports a NirSoft SkypeLogView text export into
// message-store: one conversation per Skype chat, one message per record, and
// one conversation day per chat per day, naming the journal-store transcript
// (source skype, source_ref "<chat>/<day>") that held it.
//
//	message-import-skype -log skype-log.txt.gz -time-zone America/Chicago -self me_x \
//	    -people skype-people.json -dry-run
//
// -people is a JSON object from Skype name to people-store id. The store's
// address is MESSAGE_STORE_URL.
package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/importclient"
	"github.com/kayushkin/message-store/internal/skypelog"
)

const importerName = "message-import-skype"

// Platform, Source and JournalSource name where the records came from: the
// conversation and message id namespace, and the journal-store source the
// transcripts were filed under.
const (
	Platform      = "skype"
	Source        = "skype"
	JournalSource = "skype"
)

func main() {
	logPath := flag.String("log", "", "the SkypeLogView export, UTF-8 text, optionally gzipped (.gz)")
	timeZone := flag.String("time-zone", "", "IANA zone the export's clock was in (America/Chicago); required, since the export names none")
	self := flag.String("self", "", "the user's own Skype name")
	peoplePath := flag.String("people", "", "JSON object mapping Skype names to people-store ids")
	earliestYear := flag.Int("earliest-year", 2000, "drop records dated before this year (calls with a bad clock)")
	dryRun := flag.Bool("dry-run", false, "read everything and count it; send nothing")
	flag.Parse()
	if *logPath == "" || *timeZone == "" || *self == "" {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
	if err := run(*logPath, *timeZone, *self, *peoplePath, *earliestYear, settings.String(messagestore.SettingStoreURL), *dryRun, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
}

func run(logPath, timeZone, self, peoplePath string, earliestYear int, storeURL string, dryRun bool, out io.Writer) error {
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return fmt.Errorf("-time-zone: %w", err)
	}
	people, err := importclient.ReadStringMap(peoplePath)
	if err != nil {
		return fmt.Errorf("-people: %w", err)
	}
	file, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(logPath, ".gz") {
		unzipped, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer unzipped.Close()
		reader = unzipped
	}
	records, err := skypelog.Parse(reader, location)
	if err != nil {
		return err
	}
	conversations, skipped := skypelog.Conversations(records, skypelog.Options{Self: self, EarliestYear: earliestYear})
	fmt.Fprintf(out, "read %d records into %d conversations; left out %d with no time, %d with a bad clock, %d empty\n",
		len(records), len(conversations), skipped.NoTime, skipped.BadClock, skipped.EmptyChats)
	client := importclient.New(storeURL, dryRun)
	if err := client.Begin(importerName, "a SkypeLogView export"); err != nil {
		return err
	}
	if err := send(client, conversations, location, self, people); err != nil {
		return err
	}
	if err := client.Finish(); err != nil {
		return err
	}
	fmt.Fprintln(out, client.Report())
	return nil
}

// MessageSourceRef is a record's id in the store. SkypeLogView's Record
// Number is the only key the export gives a record, and it counts records in
// this one export: another export of the same history numbers them afresh,
// and would be a second copy, not an update.
func MessageSourceRef(record skypelog.Record) string {
	return "skypelogview-record-" + strconv.Itoa(record.Number)
}

func send(sink importclient.Sink, conversations []skypelog.Conversation, location *time.Location, self string, people map[string]string) error {
	seenRecords := map[int]bool{}
	for _, conversation := range conversations {
		imported := messagestore.ImportConversation{Platform: Platform, Source: Source, SourceRef: conversation.Key,
			Title: conversation.Title, IsGroup: conversation.IsGroup}
		for _, participant := range conversation.Participants {
			imported.Participants = append(imported.Participants, messagestore.Participant{Handle: participant.UserName,
				DisplayName: participant.DisplayName, PersonID: people[participant.UserName], IsSelf: participant.UserName == self})
		}
		if err := sink.AddConversation(imported); err != nil {
			return err
		}
		var days []string
		for _, record := range conversation.Records {
			if seenRecords[record.Number] {
				return fmt.Errorf("record number %d appears twice; it cannot key a message", record.Number)
			}
			seenRecords[record.Number] = true
			message := messagestore.ImportMessage{BodySource: "platform", ConversationSource: Source, ConversationSourceRef: conversation.Key,
				SentAt: record.Time.UnixMilli(), SenderHandle: record.UserName, Kind: "text", Body: strings.TrimSpace(record.Message),
				Source: Source, SourceRef: MessageSourceRef(record)}
			if message.Body == "" && record.ActionType != "Chat Message" {
				// A call, kept because it has a duration.
				message.Kind = "call"
				message.CallSeconds = skypelog.DurationSeconds(record.Duration)
			}
			message.SourceHash = importclient.MessageHash(message)
			if err := sink.AddMessage(message); err != nil {
				return err
			}
			day := record.Time.In(location).Format(time.DateOnly)
			if len(days) == 0 || days[len(days)-1] != day {
				days = append(days, day)
			}
		}
		for _, day := range days {
			if err := sink.AddConversationDay(messagestore.ImportConversationDay{ConversationSource: Source, ConversationSourceRef: conversation.Key,
				Day: day, TimeZone: location.String(), JournalSource: JournalSource, JournalSourceRef: conversation.Key + "/" + day}); err != nil {
				return err
			}
		}
	}
	return nil
}
