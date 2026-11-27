package androidmessages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

const contacts = "Row: 0 mimetype=vnd.android.cursor.item/phone_v2, display_name=Carol C, data1=(202) 555-0100, data4=+12025550100\n"

// Carol is +12025550100; the user is +12025550199. Dave (+12025550101) is
// in a group with them. 1700000000 is 2023-11-14 22:13:20 UTC.
func phones(t *testing.T) []Phone {
	old, newer := t.TempDir(), t.TempDir()
	sms := "Row: 0 _id=1, address=(202) 555-0100, date=1700000000123, type=1, body=hello\n" +
		"Row: 1 _id=2, address=+12025550100, date=1700000060000, type=2, body=hi Carol\n"
	write(t, old, "sms.txt", sms)
	write(t, old, "mms.txt", "Row: 0 _id=9, date=1700000100, msg_box=1, m_id=abc, tr_id=NULL, sub=NULL\n")
	write(t, old, "calls.txt", "Row: 0 _id=1, number=2025550100, date=1700000200000, duration=125, type=2\n")
	write(t, old, "contacts.txt", contacts)

	write(t, newer, "sms.txt", sms+"Row: 2 _id=3, address=+12025550100, date=1700000300000, type=1, body=your verification code is 123456\n")
	write(t, newer, "mms.txt", "Row: 0 _id=4, date=1700000100, msg_box=1, m_id=abc, tr_id=NULL, sub=NULL\n"+
		"Row: 1 _id=5, date=1700000400, msg_box=2, m_id=def, tr_id=NULL, sub=NULL\n")
	write(t, newer, "mms_part.txt", "Row: 0 _id=10, mid=4, seq=0, ct=image/png, text=NULL\n"+
		"Row: 1 _id=11, mid=4, seq=1, ct=text/plain, text=look\n"+
		"Row: 2 _id=12, mid=5, seq=0, ct=text/plain, text=group hi\n")
	write(t, newer, "mms_addr.txt", "== msg 4\nRow: 0 _id=1, msg_id=4, address=+12025550100, type=137\nRow: 1 _id=2, msg_id=4, address=+12025550199, type=151\n"+
		"== msg 5\nRow: 0 _id=3, msg_id=5, address=+12025550199, type=137\nRow: 1 _id=4, msg_id=5, address=+12025550100, type=151\nRow: 2 _id=5, msg_id=5, address=2025550101, type=151\n")
	write(t, newer, "mms_parts/10.png", "not really a png")
	write(t, newer, "calls.txt", "Row: 0 _id=7, number=+12025550100, date=1700000200000, duration=125, type=2\n")
	write(t, newer, "contacts.txt", contacts)
	return []Phone{{Tag: "old", Name: "Old Phone", Dir: old}, {Tag: "new", Name: "New Phone", Dir: newer}}
}

func TestReadKeepsEachMessageOnceAndGroupsTheConversations(t *testing.T) {
	archive, err := Read(phones(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(archive.SelfNumbers, ",") != "+12025550199" {
		t.Errorf("self numbers = %v", archive.SelfNumbers)
	}
	// 2 SMS shared, 1 new SMS, the shared MMS (read from the new phone, counted for the old) and the group MMS.
	if len(archive.Messages) != 5 || len(archive.Calls) != 1 {
		t.Fatalf("messages = %d, calls = %d", len(archive.Messages), len(archive.Calls))
	}
	if archive.Counts[0].DuplicateMessages != 1 || archive.Counts[1].DuplicateMessages != 2 || archive.Counts[1].DuplicateCalls != 1 {
		t.Errorf("counts = %+v", archive.Counts)
	}
	keys := map[string]bool{}
	for _, message := range archive.Messages {
		if message.DedupeKey == "" || keys[message.DedupeKey] {
			t.Errorf("message at %s has dedupe key %q, empty or taken", message.Time, message.DedupeKey)
		}
		keys[message.DedupeKey] = true
		if message.Picture && !strings.HasPrefix(message.DedupeKey, "mms-id|") {
			t.Errorf("a picture message with a carrier id is keyed %q", message.DedupeKey)
		}
	}
	if archive.Calls[0].DedupeKey == "" || strings.Join(archive.Calls[0].Phones, ",") != "old,new" {
		t.Errorf("call = %+v", archive.Calls[0])
	}
	again, err := Read(phones(t))
	if err != nil {
		t.Fatal(err)
	}
	for index, message := range again.Messages {
		if message.DedupeKey != archive.Messages[index].DedupeKey {
			t.Errorf("message %d is keyed %q on one run and %q on the next", index, archive.Messages[index].DedupeKey, message.DedupeKey)
		}
	}
	conversations, skipped := Conversations(archive)
	if len(conversations) != 2 || skipped.UserNeverWrote != 0 {
		t.Fatalf("conversations = %d, skipped %+v", len(conversations), skipped)
	}
	direct, group := conversations[0], conversations[1]
	if direct.Key() != "+12025550100" || len(direct.Messages) != 4 || len(direct.Calls) != 1 {
		t.Errorf("direct = %s, %d messages, %d calls", direct.Key(), len(direct.Messages), len(direct.Calls))
	}
	if group.Key() != "+12025550100,+12025550101" || len(group.Messages) != 1 {
		t.Errorf("group = %s, %d messages", group.Key(), len(group.Messages))
	}
	if attachments := direct.Messages[2].Attachments; len(attachments) != 1 || !attachments[0].IsPicture() || attachments[0].Sha256 == "" {
		t.Errorf("the shared picture message's attachments = %+v", attachments)
	}
}

func TestSecretKind(t *testing.T) {
	for text, want := range map[string]string{
		"Your verification code is 482913":      "a login code",
		"G-123456 is your Google code":          "a login code",
		"the wifi password is hunter22":         "a password",
		"482913\nUse this code to sign in":      "a login code",
		"The temporary pin is:\n\n\n12UPPCNW":   "a login code",
		"see you at 1234 Main St at 5":          "",
		"I forgot my password again lol":        "",
		"can you send me the code for the gate": "",
	} {
		if got := SecretKind(text); got != want {
			t.Errorf("SecretKind(%q) = %q, want %q", text, got, want)
		}
	}
}
