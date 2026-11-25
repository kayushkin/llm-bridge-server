// Command message-import-android imports the text messages, picture messages
// and calls copied off one or more Android phones into message-store, one row
// per message.
//
//	message-import-android \
//	    -phone 'old|Old Phone|<dir>' -phone 'new|New Phone|<dir>' \
//	    -self-number +12025550199 -self-name 'Me Self' \
//	    -time-zones 'Europe/Lisbon,2021-07-11=America/Chicago' \
//	    -fold-into-source <earlier journal source> -dry-run
//
// Each -phone is "tag|name|folder": the folder holds the phone's content
// query exports (see internal/androidmessages). Phones carry the same history
// from one to the next, so every message and call is kept once, however many
// phones held it, and its source_ref is the hash of the key that said so.
//
// The rules are journal-import-android-messages' rules: only direct
// conversations and groups of at most eight people the user wrote in are
// imported, and a message that is a login code or a password keeps no text.
// A call joins the direct conversation with its number on any day, texts or
// none; a phone number the user kept no texts with gets a direct conversation
// of its calls alone. A call from a private or unknown number is left out and
// counted.
//
// A conversation is source sms, source_ref the other people's E.164 numbers,
// sorted and joined by commas. Each number is named through people-store and
// each picture is cited by the photo-store id journal-import-android-messages
// gave it; nothing is uploaded. A conversation day goes for every day that
// importer wrote a transcript for, under the same journal source and
// source_ref: -journal-source, or -fold-into-source when that earlier import
// already held the day.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/androidmessages"
	"github.com/kayushkin/message-store/internal/importclient"
	"github.com/kayushkin/message-store/internal/photolookup"
	"github.com/kayushkin/message-store/internal/residencezones"
)

const importerName = "message-import-android"

// Platform and Source are what every row this command writes is filed under.
const (
	Platform = "sms"
	Source   = "sms"
)

type phoneList []androidmessages.Phone

func (phones *phoneList) String() string { return fmt.Sprint(*phones) }

func (phones *phoneList) Set(value string) error {
	parts := strings.SplitN(value, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("%q is not tag|name|folder", value)
	}
	*phones = append(*phones, androidmessages.Phone{Tag: parts[0], Name: parts[1], Dir: parts[2]})
	return nil
}

func main() {
	var phones phoneList
	flag.Var(&phones, "phone", "a phone's export as tag|name|folder (repeat, oldest history first, in the order journal-import-android-messages read them)")
	selfNumber := flag.String("self-number", "", "the user's own number, E.164: the handle of every message and call the user sent")
	selfName := flag.String("self-name", "", "the user's display name in each conversation")
	timeZones := flag.String("time-zones", "", "where the user lived: first zone, then day=zone for each move (Europe/Lisbon,2021-07-11=America/Chicago)")
	journalSource := flag.String("journal-source", "android-messages", "the journal-store source journal-import-android-messages filed new transcripts under")
	foldIntoSource := flag.String("fold-into-source", "", "the earlier journal-store source whose transcripts that importer extended in place")
	dryRun := flag.Bool("dry-run", false, "read everything and count what would be sent; send nothing")
	flag.Parse()
	if len(phones) == 0 || *timeZones == "" || androidmessages.E164(*selfNumber) != *selfNumber || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	zones, err := residencezones.Parse(*timeZones)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: -time-zones: %v\n", importerName, err)
		os.Exit(2)
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: settings: %v\n", importerName, err)
		os.Exit(2)
	}
	run := runner{
		phones: phones, selfNumber: *selfNumber, selfName: *selfName, zones: zones,
		journalSource: *journalSource, foldIntoSource: *foldIntoSource, dryRun: *dryRun,
		storeURL:        settings.String(messagestore.SettingStoreURL),
		journalStoreURL: strings.TrimRight(settings.String(messagestore.SettingJournalStoreURL), "/"),
		peopleStoreURL:  strings.TrimRight(settings.String(messagestore.SettingPeopleStoreURL), "/"),
		photos:          photolookup.New(settings.String(messagestore.SettingPhotoStoreURL)),
	}
	if err := run.run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
}

type runner struct {
	phones                        []androidmessages.Phone
	selfNumber, selfName          string
	zones                         residencezones.Zones
	journalSource, foldIntoSource string
	dryRun                        bool
	storeURL, journalStoreURL     string
	peopleStoreURL                string
	photos                        *photolookup.Lookup
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// getJSON reads address into out; found is false on a 404.
func getJSON(address string, out any) (found bool, err error) {
	response, err := httpClient.Get(address)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s answered %d: %s", address, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, json.Unmarshal(body, out)
}

func (run runner) run(out io.Writer) error {
	archive, err := androidmessages.Read(run.phones)
	if err != nil {
		return err
	}
	for _, counts := range archive.Counts {
		fmt.Fprintf(out, "%s: read %d SMS, %d MMS, %d calls; dropped %d messages and %d calls another phone or row already held; left out %d drafts or failed messages, %d MMS no phone exported in full; %d attachment files missing\n",
			counts.Tag, counts.SMS, counts.MMS, counts.Calls, counts.DuplicateMessages, counts.DuplicateCalls, counts.NotSentOrReceived,
			counts.PictureMessagesWithoutDetails, counts.AttachmentFilesMissing)
	}
	fmt.Fprintf(out, "kept %d messages and %d calls once each; %d numbers of the user's own seen on picture messages\n",
		len(archive.Messages), len(archive.Calls), len(archive.SelfNumbers))
	conversations, skipped := androidmessages.Conversations(archive)
	callsOnly, callsOnlyCalls := 0, 0
	for _, conversation := range conversations {
		if conversation.CallsOnly {
			callsOnly++
			callsOnlyCalls += len(conversation.Calls)
		}
	}
	fmt.Fprintf(out, "%d conversations kept; left out: %d with a short code or email address (%d messages), %d of more than %d people (%d messages), %d the user never wrote in (%d messages), %d messages with no one else; %d calls from no phone number, %d blocked calls; %d of the kept are numbers with calls and no texts (%d calls)\n",
		len(conversations), skipped.NotPhoneNumber, skipped.NotPhoneNumberMessages, skipped.TooBig, androidmessages.MaximumMembers, skipped.TooBigMessages,
		skipped.UserNeverWrote, skipped.UserNeverWroteMessages, skipped.NoOneElseMessages, skipped.CallsNotPhoneNumber, skipped.BlockedCalls, callsOnly, callsOnlyCalls)

	people, err := run.resolvePeople(append([]string{run.selfNumber}, othersOf(conversations)...))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "people-store: %d of the numbers belong to someone\n", len(people))
	folded, err := run.foldedRefs()
	if err != nil {
		return err
	}
	pictures, err := run.pictureIDs(conversations)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, run.photos.Report())

	client := importclient.New(run.storeURL, run.dryRun)
	if err := client.Begin(importerName, fmt.Sprintf("%d Android phones' texts and calls", len(run.phones))); err != nil {
		return err
	}
	options := options{selfNumber: run.selfNumber, selfName: run.selfName, zones: run.zones, journalSource: run.journalSource,
		foldIntoSource: run.foldIntoSource, folded: folded, people: people, contactNames: archive.ContactNames, pictures: pictures}
	counts, err := send(client, conversations, options)
	if err != nil {
		return err
	}
	if err := client.Finish(); err != nil {
		return err
	}
	fmt.Fprintf(out, "%d texts (%d kept no text as a login code or password), %d calls; %d transcript days, %d of them under %s\n",
		counts.messages, counts.secrets, counts.calls, counts.days, counts.foldedDays, run.foldIntoSource)
	fmt.Fprintln(out, client.Report())
	return nil
}

func othersOf(conversations []*androidmessages.Conversation) []string {
	var numbers []string
	seen := map[string]bool{}
	for _, conversation := range conversations {
		for _, number := range conversation.Others {
			if !seen[number] {
				seen[number] = true
				numbers = append(numbers, number)
			}
		}
	}
	return numbers
}

// person is who a number belongs to in people-store.
type person struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func (run runner) resolvePeople(numbers []string) (map[string]person, error) {
	people := map[string]person{}
	for _, number := range numbers {
		var resolution struct {
			Person person `json:"person"`
		}
		found, err := getJSON(run.peopleStoreURL+"/identities/resolve?"+url.Values{"kind": {"phone"}, "value": {number}}.Encode(), &resolution)
		if err != nil {
			return nil, err
		}
		if found {
			if resolution.Person.ID == "" {
				return nil, fmt.Errorf("people-store resolved a number to a person with no id")
			}
			people[number] = resolution.Person
		}
	}
	return people, nil
}

// foldedRefs are the source_refs journal-store holds under -fold-into-source,
// deleted ones too, as journal-import-android-messages read them.
func (run runner) foldedRefs() (map[string]bool, error) {
	refs := map[string]bool{}
	if run.foldIntoSource == "" {
		return refs, nil
	}
	for offset := 0; ; offset += 500 {
		var page []struct {
			Source    string `json:"source"`
			SourceRef string `json:"source_ref"`
		}
		query := url.Values{"source": {run.foldIntoSource}, "include_deleted": {"true"}, "limit": {"500"}, "offset": {fmt.Sprint(offset)}}
		if _, err := getJSON(run.journalStoreURL+"/entries?"+query.Encode(), &page); err != nil {
			return nil, err
		}
		for _, entry := range page {
			if entry.Source != run.foldIntoSource {
				return nil, fmt.Errorf("journal-store answered ?source=%s with an entry of source %q", run.foldIntoSource, entry.Source)
			}
			refs[entry.SourceRef] = true
		}
		if len(page) < 500 {
			return refs, nil
		}
	}
}

// pictureIDs asks photo-store for every picture's id, by the file's hash.
func (run runner) pictureIDs(conversations []*androidmessages.Conversation) (map[string]string, error) {
	ids := map[string]string{}
	for _, conversation := range conversations {
		for _, message := range conversation.Messages {
			for _, attachment := range message.Attachments {
				if !attachment.IsPicture() || attachment.Path == "" {
					continue
				}
				if _, asked := ids[attachment.Sha256]; asked {
					continue
				}
				original, err := os.ReadFile(attachment.Path)
				if err != nil {
					return nil, err
				}
				if ids[attachment.Sha256], err = run.photos.PhotoID(original); err != nil {
					return nil, err
				}
			}
		}
	}
	return ids, nil
}

// options are what send needs besides the conversations.
type options struct {
	selfNumber, selfName          string
	zones                         residencezones.Zones
	journalSource, foldIntoSource string
	// folded are the source_refs the -fold-into-source import holds.
	folded       map[string]bool
	people       map[string]person
	contactNames map[string][]string
	// pictures are photo-store ids by the file's sha256; "" when photo-store
	// holds none to link.
	pictures map[string]string
}

// name is a number's display name: its person, else the names the phones'
// contact lists saved it under, else nothing.
func (o options) name(number string) string {
	if found, known := o.people[number]; known {
		return found.DisplayName
	}
	return strings.Join(o.contactNames[number], " / ")
}

type sendCounts struct {
	messages, secrets, calls, days, foldedDays int
}

// hashRef is a dedupe key as a source_ref: keys hold numbers and text, so
// only their hash leaves this command.
func hashRef(prefix, key string) string {
	digest := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(digest[:])
}

// send hands every conversation, its messages, calls and transcript days to
// the sink.
func send(sink importclient.Sink, conversations []*androidmessages.Conversation, o options) (sendCounts, error) {
	var counts sendCounts
	for _, conversation := range conversations {
		key := conversation.Key()
		participants := []messagestore.Participant{{Handle: o.selfNumber, DisplayName: o.selfName, PersonID: o.people[o.selfNumber].ID, IsSelf: true}}
		var names []string
		for _, number := range conversation.Others {
			participants = append(participants, messagestore.Participant{Handle: number, DisplayName: o.name(number), PersonID: o.people[number].ID})
			name := o.name(number)
			if name == "" {
				name = number
			}
			names = append(names, name)
		}
		if err := sink.AddConversation(messagestore.ImportConversation{Platform: Platform, Source: Source, SourceRef: key,
			Title: strings.Join(names, ", "), IsGroup: len(conversation.Others) > 1, Participants: participants}); err != nil {
			return counts, err
		}
		// days are the journal importer's transcript days, each with the
		// zone its messages were filed by.
		days := map[string]string{}
		for _, message := range conversation.Messages {
			zone := o.zones.At(message.Time)
			days[message.Time.In(zone).Format(time.DateOnly)] = zone.String()
			row, secret := o.message(key, message)
			if secret {
				counts.secrets++
			}
			if err := sink.AddMessage(row); err != nil {
				return counts, err
			}
			counts.messages++
		}
		for _, call := range conversation.Calls {
			if err := sink.AddMessage(o.call(key, call)); err != nil {
				return counts, err
			}
			counts.calls++
		}
		ordered := make([]string, 0, len(days))
		for day := range days {
			ordered = append(ordered, day)
		}
		sort.Strings(ordered)
		for _, day := range ordered {
			ref := key + "/" + day
			journalSource := o.journalSource
			if o.folded[ref] {
				journalSource = o.foldIntoSource
				counts.foldedDays++
			}
			if err := sink.AddConversationDay(messagestore.ImportConversationDay{ConversationSource: Source, ConversationSourceRef: key,
				Day: day, TimeZone: days[day], JournalSource: journalSource, JournalSourceRef: ref}); err != nil {
				return counts, err
			}
			counts.days++
		}
	}
	return counts, nil
}

// secretBody is what a login code or a password keeps of its text.
func secretBody(kind string) string { return "[" + kind + ", left out]" }

func (o options) message(conversationKey string, message *androidmessages.Message) (messagestore.ImportMessage, bool) {
	sender := message.Sender
	if message.FromSelf {
		sender = o.selfNumber
	}
	body := strings.TrimSpace(message.Text)
	secret := false
	if kind := androidmessages.SecretKind(message.Text); message.Text != "" && kind != "" {
		body, secret = secretBody(kind), true
	}
	attachments := []messagestore.Attachment{}
	for _, attachment := range message.Attachments {
		item := messagestore.Attachment{MimeType: attachment.ContentType}
		if attachment.IsPicture() && attachment.Path != "" {
			item.PhotoID = o.pictures[attachment.Sha256]
		}
		attachments = append(attachments, item)
	}
	kind := "text"
	if body == "" && len(attachments) > 0 {
		kind = "media"
	}
	row := messagestore.ImportMessage{BodySource: "platform", ConversationSource: Source, ConversationSourceRef: conversationKey, SentAt: message.Time.UnixMilli(),
		SenderHandle: sender, Kind: kind, Body: body, Attachments: attachments, Source: Source, SourceRef: hashRef("message:", message.DedupeKey)}
	row.SourceHash = importclient.MessageHash(row)
	return row, secret
}

// callBodies describe each call-log type; a call's length is call_seconds.
var callBodies = map[androidmessages.CallType]string{
	androidmessages.CallIncoming:  "incoming call",
	androidmessages.CallOutgoing:  "outgoing call",
	androidmessages.CallMissed:    "missed call",
	androidmessages.CallVoicemail: "voicemail",
	androidmessages.CallRejected:  "declined call",
}

func (o options) call(conversationKey string, call *androidmessages.Call) messagestore.ImportMessage {
	sender := call.Number
	if call.Type == androidmessages.CallOutgoing {
		sender = o.selfNumber
	}
	body, known := callBodies[call.Type]
	if !known {
		body = fmt.Sprintf("call (call log type %d)", call.Type)
	}
	row := messagestore.ImportMessage{BodySource: "platform", ConversationSource: Source, ConversationSourceRef: conversationKey, SentAt: call.Time.UnixMilli(),
		SenderHandle: sender, Kind: "call", Body: body, Attachments: []messagestore.Attachment{}, CallSeconds: call.DurationSeconds,
		Source: Source, SourceRef: hashRef("call:", call.DedupeKey)}
	row.SourceHash = importclient.MessageHash(row)
	return row
}
