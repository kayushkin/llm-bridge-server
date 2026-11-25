// Package androidmessages reads the text messages, picture messages and call
// log copied off Android phones with `adb shell content query`, drops every
// message two phones both kept, and groups what is left into the
// conversations the user took part in.
//
// A phone's folder holds, each the output of one content query:
//
//	sms.txt        content://sms
//	mms.txt        content://mms
//	mms_part.txt   content://mms/part
//	mms_addr.txt   content://mms/<id>/addr for every message, each headed "== msg <id>"
//	mms_parts/     each attachment part's file, named <part _id>.<extension>
//	calls.txt      content://call_log/calls
//	contacts.txt   content://com.android.contacts/data
//
// mms_part.txt, mms_addr.txt and mms_parts/ may be missing: that phone's
// picture messages then cannot be read, and count only as a message another
// phone may also hold.
package androidmessages

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/message-store/internal/contentquery"
)

// Phone is one phone's exported folder.
type Phone struct {
	// Tag is a short name for the phone (oldphone).
	Tag string
	// Name is the phone's model as a person would say it (Galaxy Z Flip 3).
	Name string
	Dir  string
}

// Attachment is one part of a picture message besides its text.
type Attachment struct {
	ContentType string
	// Path is the part's file, "" when the export lacks it.
	Path string
	// Sha256 is of the file, "" when there is none.
	Sha256 string
}

// IsPicture says whether the attachment is an image or a video, the kinds
// photo-store keeps.
func (attachment Attachment) IsPicture() bool {
	return strings.HasPrefix(attachment.ContentType, "image/") || strings.HasPrefix(attachment.ContentType, "video/")
}

// Message is one text or picture message, once, however many phones kept it.
type Message struct {
	// Phones are the tags of the phones that kept it, in the order read.
	Phones []string
	// Others are the numbers of everyone in the conversation but the user,
	// sorted: one for a direct conversation. A number that is not a phone
	// number (a short code, an email address) is kept as written.
	Others []string
	// Sender is who sent it, "" when the user did.
	Sender   string
	FromSelf bool
	Time     time.Time
	Text     string
	// Attachments are a picture message's parts besides text, in order.
	Attachments []Attachment
	Picture     bool // an MMS, not an SMS
	// DedupeKey is the first key that said two rows are this one message,
	// from the copy kept: the same on every run over the same phones.
	DedupeKey string
}

// Call is one call-log row, once, however many phones kept it.
type Call struct {
	Phones []string
	// Number is the other side, E.164 when it is a phone number.
	Number          string
	Time            time.Time
	DurationSeconds int
	Type            CallType
	// DedupeKey is the number, second, duration and type that say two rows
	// are this one call.
	DedupeKey string
}

// CallType is the call log's type column.
type CallType int

// The call log's types (android.provider.CallLog.Calls).
const (
	CallIncoming  CallType = 1
	CallOutgoing  CallType = 2
	CallMissed    CallType = 3
	CallVoicemail CallType = 4
	CallRejected  CallType = 5
	CallBlocked   CallType = 6
)

// PhoneCounts is what was read from one phone.
type PhoneCounts struct {
	Tag string
	// SMS, MMS and Calls are the rows read.
	SMS, MMS, Calls int
	// DuplicateMessages and DuplicateCalls are rows an earlier phone (or an
	// earlier row of this one) already held, dropped.
	DuplicateMessages, DuplicateCalls int
	// NotSentOrReceived are drafts, failed and queued messages, left out.
	NotSentOrReceived int
	// PictureMessagesWithoutDetails are MMS rows of a phone whose parts and
	// addresses were not exported, that no other phone held either.
	PictureMessagesWithoutDetails int
	// AttachmentFilesMissing are attachment parts with no file in mms_parts/.
	AttachmentFilesMissing int
	First, Last            time.Time
}

// Archive is every phone's messages and calls, each once.
type Archive struct {
	Messages []*Message
	Calls    []*Call
	// ContactNames are the names the phones' contact lists saved each
	// number under, sorted.
	ContactNames map[string][]string
	// SelfNumbers are the user's own numbers: the senders of the picture
	// messages the user sent.
	SelfNumbers []string
	Counts      []PhoneCounts
}

// selfToken is what Android writes in place of the user's own number on a
// picture message being sent.
const selfToken = "insert-address-token"

// MMS address types (PduHeaders): from, to, cc, bcc.
const (
	addressFrom = "137"
	addressTo   = "151"
	addressCc   = "130"
	addressBcc  = "129"
)

var nonDialable = regexp.MustCompile(`[^\d+]`)

// E164 is a North American number written any way as +1NXXNXXXXXX, a number
// that starts with + as itself, and "" for anything else (short codes,
// email addresses).
func E164(number string) string {
	digits := nonDialable.ReplaceAllString(number, "")
	switch {
	case strings.HasPrefix(digits, "+") && len(digits) > 1 && !strings.Contains(digits[1:], "+"):
		return digits
	case len(digits) == 10 && !strings.Contains(digits, "+"):
		return "+1" + digits
	case len(digits) == 11 && strings.HasPrefix(digits, "1"):
		return "+" + digits
	}
	return ""
}

// handle is a number as a conversation key: E.164 when it is a phone number,
// otherwise as written.
func handle(address string) string {
	if number := E164(address); number != "" {
		return number
	}
	return strings.TrimSpace(address)
}

// rawPhone is one phone's rows, read before anything is merged.
type rawPhone struct {
	phone     Phone
	sms       []contentquery.Row
	mms       []contentquery.Row
	parts     map[string][]contentquery.Row // by message _id
	addresses map[string][]contentquery.Row // by message _id
	partFiles map[string]string             // part _id to file
	calls     []contentquery.Row
	contacts  []contentquery.Row
	detailed  bool
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func readPhone(phone Phone) (*rawPhone, error) {
	raw := &rawPhone{phone: phone}
	var err error
	if raw.sms, err = contentquery.ReadFile(filepath.Join(phone.Dir, "sms.txt")); err != nil {
		return nil, err
	}
	if raw.mms, err = contentquery.ReadFile(filepath.Join(phone.Dir, "mms.txt")); err != nil {
		return nil, err
	}
	if raw.calls, err = contentquery.ReadFile(filepath.Join(phone.Dir, "calls.txt")); err != nil {
		return nil, err
	}
	if raw.contacts, err = contentquery.ReadFile(filepath.Join(phone.Dir, "contacts.txt")); err != nil {
		return nil, err
	}
	partsPath, addressesPath := filepath.Join(phone.Dir, "mms_part.txt"), filepath.Join(phone.Dir, "mms_addr.txt")
	hasParts, err := exists(partsPath)
	if err != nil {
		return nil, err
	}
	hasAddresses, err := exists(addressesPath)
	if err != nil {
		return nil, err
	}
	if hasParts != hasAddresses {
		return nil, fmt.Errorf("%s: mms_part.txt and mms_addr.txt come together; only one is here", phone.Dir)
	}
	if !hasParts {
		return raw, nil
	}
	raw.detailed = true
	parts, err := contentquery.ReadFile(partsPath)
	if err != nil {
		return nil, err
	}
	raw.parts = map[string][]contentquery.Row{}
	for _, part := range parts {
		raw.parts[part.Get("mid")] = append(raw.parts[part.Get("mid")], part)
	}
	if raw.addresses, err = contentquery.ReadBlocksFile(addressesPath); err != nil {
		return nil, err
	}
	for id, rows := range raw.addresses {
		for _, row := range rows {
			if row.Get("msg_id") != id {
				return nil, fmt.Errorf("%s: block msg %s holds an address of msg %s", addressesPath, id, row.Get("msg_id"))
			}
		}
	}
	raw.partFiles = map[string]string{}
	files, err := os.ReadDir(filepath.Join(phone.Dir, "mms_parts"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, file := range files {
		id := strings.SplitN(file.Name(), ".", 2)[0]
		if previous, twice := raw.partFiles[id]; twice {
			return nil, fmt.Errorf("%s: two files for part %s: %s and %s", phone.Dir, id, previous, file.Name())
		}
		raw.partFiles[id] = filepath.Join(phone.Dir, "mms_parts", file.Name())
	}
	return raw, nil
}

func integer(row contentquery.Row, column string) (int64, error) {
	value, err := strconv.ParseInt(row.Get(column), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", column, row.Get(column), err)
	}
	return value, nil
}

func hashFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// Read reads every phone and keeps each message and call once. Phones are
// read in the order given; a message's Phones say every phone that held it.
func Read(phones []Phone) (*Archive, error) {
	var raws []*rawPhone
	for _, phone := range phones {
		raw, err := readPhone(phone)
		if err != nil {
			return nil, err
		}
		raws = append(raws, raw)
	}
	archive := &Archive{ContactNames: map[string][]string{}}
	selfNumbers := map[string]bool{}
	for _, raw := range raws {
		if !raw.detailed {
			continue
		}
		for _, row := range raw.mms {
			if row.Get("msg_box") != "2" {
				continue
			}
			for _, address := range raw.addresses[row.Get("_id")] {
				if address.Get("type") == addressFrom && address.Get("address") != selfToken {
					selfNumbers[handle(address.Get("address"))] = true
				}
			}
		}
	}
	for number := range selfNumbers {
		archive.SelfNumbers = append(archive.SelfNumbers, number)
	}
	sort.Strings(archive.SelfNumbers)

	seen := map[string]*Message{}
	remember := func(message *Message, keys []string, counts *PhoneCounts, tag string) {
		for _, key := range keys {
			if earlier := seen[key]; earlier != nil {
				counts.DuplicateMessages++
				if !containsString(earlier.Phones, tag) {
					earlier.Phones = append(earlier.Phones, tag)
				}
				fillMissingFiles(earlier, message)
				for _, other := range keys {
					if seen[other] == nil {
						seen[other] = earlier
					}
				}
				return
			}
		}
		for _, key := range keys {
			seen[key] = message
		}
		message.DedupeKey = keys[0]
		archive.Messages = append(archive.Messages, message)
	}
	countsByTag := map[string]*PhoneCounts{}
	for _, raw := range raws {
		tag := raw.phone.Tag
		counts := &PhoneCounts{Tag: tag, SMS: len(raw.sms), MMS: len(raw.mms), Calls: len(raw.calls)}
		countsByTag[tag] = counts
		for index, row := range raw.sms {
			message, keys, err := smsMessage(row, tag)
			if err != nil {
				return nil, fmt.Errorf("%s sms.txt row %d: %w", raw.phone.Dir, index, err)
			}
			if message == nil {
				counts.NotSentOrReceived++
				continue
			}
			counts.see(message.Time)
			remember(message, keys, counts, tag)
		}
		if raw.detailed {
			for index, row := range raw.mms {
				message, keys, missing, err := mmsMessage(raw, row, selfNumbers)
				if err != nil {
					return nil, fmt.Errorf("%s mms.txt row %d: %w", raw.phone.Dir, index, err)
				}
				counts.AttachmentFilesMissing += missing
				if message == nil {
					counts.NotSentOrReceived++
					continue
				}
				counts.see(message.Time)
				remember(message, keys, counts, tag)
			}
		}
		for _, row := range raw.contacts {
			if row.Get("mimetype") != "vnd.android.cursor.item/phone_v2" || strings.TrimSpace(row.Get("display_name")) == "" {
				continue
			}
			number := E164(row.Get("data4"))
			if number == "" {
				number = E164(row.Get("data1"))
			}
			if number != "" && !containsString(archive.ContactNames[number], strings.TrimSpace(row.Get("display_name"))) {
				archive.ContactNames[number] = append(archive.ContactNames[number], strings.TrimSpace(row.Get("display_name")))
			}
		}
	}
	// A phone without parts and addresses can only say which of its picture
	// messages another phone also held.
	for _, raw := range raws {
		if raw.detailed {
			continue
		}
		counts := countsByTag[raw.phone.Tag]
		for _, row := range raw.mms {
			found := false
			for _, key := range mmsIDKeys(row) {
				if earlier := seen[key]; earlier != nil {
					found = true
					if !containsString(earlier.Phones, raw.phone.Tag) {
						earlier.Phones = append(earlier.Phones, raw.phone.Tag)
					}
					break
				}
			}
			if found {
				counts.DuplicateMessages++
			} else {
				counts.PictureMessagesWithoutDetails++
			}
		}
	}
	seenCalls := map[string]*Call{}
	for _, raw := range raws {
		counts := countsByTag[raw.phone.Tag]
		for index, row := range raw.calls {
			call, err := callFrom(row, raw.phone.Tag)
			if err != nil {
				return nil, fmt.Errorf("%s calls.txt row %d: %w", raw.phone.Dir, index, err)
			}
			key := fmt.Sprintf("%s|%d|%d|%d", call.Number, call.Time.Unix(), call.DurationSeconds, call.Type)
			if earlier := seenCalls[key]; earlier != nil {
				counts.DuplicateCalls++
				if !containsString(earlier.Phones, raw.phone.Tag) {
					earlier.Phones = append(earlier.Phones, raw.phone.Tag)
				}
				continue
			}
			seenCalls[key] = call
			call.DedupeKey = key
			archive.Calls = append(archive.Calls, call)
		}
		archive.Counts = append(archive.Counts, *counts)
	}
	for number := range archive.ContactNames {
		sort.Strings(archive.ContactNames[number])
	}
	sort.SliceStable(archive.Messages, func(i, j int) bool { return archive.Messages[i].Time.Before(archive.Messages[j].Time) })
	sort.SliceStable(archive.Calls, func(i, j int) bool { return archive.Calls[i].Time.Before(archive.Calls[j].Time) })
	return archive, nil
}

func (counts *PhoneCounts) see(moment time.Time) {
	if counts.First.IsZero() || moment.Before(counts.First) {
		counts.First = moment
	}
	if moment.After(counts.Last) {
		counts.Last = moment
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// fillMissingFiles gives the kept copy of a message any attachment file a
// later copy has and it lacks.
func fillMissingFiles(kept, later *Message) {
	if len(kept.Attachments) != len(later.Attachments) {
		return
	}
	for index := range kept.Attachments {
		if kept.Attachments[index].Path == "" && later.Attachments[index].Path != "" {
			kept.Attachments[index] = later.Attachments[index]
		}
	}
}

// smsMessage is one sms row, with the keys that say two rows are one
// message: the other number, the second it was sent, which way, and the
// text. nil for a draft, a failed or a queued message.
func smsMessage(row contentquery.Row, tag string) (*Message, []string, error) {
	millis, err := integer(row, "date")
	if err != nil {
		return nil, nil, err
	}
	var fromSelf bool
	switch row.Get("type") {
	case "1":
	case "2":
		fromSelf = true
	default:
		return nil, nil, nil
	}
	other := handle(row.Get("address"))
	moment := time.UnixMilli(millis).UTC()
	text := strings.ReplaceAll(row.Get("body"), "\r", "")
	message := &Message{Phones: []string{tag}, Others: []string{other}, FromSelf: fromSelf, Time: moment, Text: text}
	if !fromSelf {
		message.Sender = other
	}
	key := fmt.Sprintf("sms|%s|%d|%t|%s", other, moment.Unix(), fromSelf, text)
	return message, []string{key}, nil
}

// mmsIDKeys are the ids a picture message keeps from phone to phone: the
// carrier's message id, else the transaction id.
func mmsIDKeys(row contentquery.Row) []string {
	if id := row.Get("m_id"); id != "" {
		return []string{"mms-id|" + id}
	}
	if id := row.Get("tr_id"); id != "" {
		return []string{"mms-transaction|" + id}
	}
	return nil
}

// mmsMessage is one mms row with its parts and addresses, keyed by its ids
// and by what it holds (sender, second, text, attachment hashes), so a copy
// whose ids differ is still found. missing counts attachment parts with no
// file. nil for a draft, a failed or a queued message.
func mmsMessage(raw *rawPhone, row contentquery.Row, selfNumbers map[string]bool) (message *Message, keys []string, missing int, err error) {
	var fromSelf bool
	switch row.Get("msg_box") {
	case "1":
	case "2":
		fromSelf = true
	default:
		return nil, nil, 0, nil
	}
	seconds, err := integer(row, "date")
	if err != nil {
		return nil, nil, 0, err
	}
	id := row.Get("_id")
	message = &Message{Phones: []string{raw.phone.Tag}, FromSelf: fromSelf, Time: time.Unix(seconds, 0).UTC(), Picture: true}
	others := map[string]bool{}
	for _, address := range raw.addresses[id] {
		value := address.Get("address")
		if value == selfToken || value == "" {
			continue
		}
		number := handle(value)
		if selfNumbers[number] {
			continue
		}
		switch address.Get("type") {
		case addressFrom:
			if !fromSelf {
				message.Sender = number
			}
			others[number] = true
		case addressTo, addressCc, addressBcc:
			others[number] = true
		default:
			return nil, nil, 0, fmt.Errorf("msg %s: address type %q", id, address.Get("type"))
		}
	}
	for number := range others {
		message.Others = append(message.Others, number)
	}
	sort.Strings(message.Others)
	parts := raw.parts[id]
	sort.SliceStable(parts, func(i, j int) bool {
		left, _ := strconv.Atoi(parts[i].Get("seq"))
		right, _ := strconv.Atoi(parts[j].Get("seq"))
		if left != right {
			return left < right
		}
		leftID, _ := strconv.Atoi(parts[i].Get("_id"))
		rightID, _ := strconv.Atoi(parts[j].Get("_id"))
		return leftID < rightID
	})
	var texts, hashes []string
	for _, part := range parts {
		contentType := strings.ToLower(part.Get("ct"))
		switch {
		case contentType == "application/smil":
		case contentType == "text/plain":
			if text := strings.ReplaceAll(part.Get("text"), "\r", ""); strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		default:
			attachment := Attachment{ContentType: contentType, Path: raw.partFiles[part.Get("_id")]}
			if attachment.Path == "" {
				missing++
			} else if attachment.Sha256, err = hashFile(attachment.Path); err != nil {
				return nil, nil, 0, err
			}
			hashes = append(hashes, contentType+":"+attachment.Sha256)
			message.Attachments = append(message.Attachments, attachment)
		}
	}
	message.Text = strings.Join(texts, "\n")
	if subject := strings.TrimSpace(row.Get("sub")); subject != "" && !strings.Contains(message.Text, subject) {
		message.Text = strings.TrimSpace(subject + "\n" + message.Text)
	}
	sender := message.Sender
	if fromSelf {
		sender = "self"
	}
	keys = append(mmsIDKeys(row), fmt.Sprintf("mms-content|%s|%s|%d|%s|%s",
		strings.Join(message.Others, ","), sender, seconds, message.Text, strings.Join(hashes, ",")))
	return message, keys, missing, nil
}

func callFrom(row contentquery.Row, tag string) (*Call, error) {
	millis, err := integer(row, "date")
	if err != nil {
		return nil, err
	}
	duration, err := integer(row, "duration")
	if err != nil {
		return nil, err
	}
	kind, err := integer(row, "type")
	if err != nil {
		return nil, err
	}
	return &Call{Phones: []string{tag}, Number: handle(row.Get("number")), Time: time.UnixMilli(millis).UTC(),
		DurationSeconds: int(duration), Type: CallType(kind)}, nil
}
