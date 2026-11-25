package discordpackage

import (
	"os"
	"path/filepath"
	"testing"
)

// writePackage lays out a synthetic Messages folder: a DM, a group DM and a
// server channel.
func writePackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.json":         `{"100":"Direct Message with carol_c","200":"None","300":"general in Some Server"}`,
		"c100/channel.json":  `{"id":"100","type":"DM","recipients":["9","1"]}`,
		"c100/messages.json": `[{"ID":12,"Timestamp":"2021-07-11 03:00:00","Contents":"after midnight","Attachments":""},{"ID":11,"Timestamp":"2021-07-10 15:00:00","Contents":"hi","Attachments":"https://cdn.discordapp.com/attachments/100/555/cat.png?ex=0&hm=abc"}]`,
		"c200/channel.json":  `{"id":"200","type":"GROUP_DM","recipients":["9","2","3"]}`,
		"c200/messages.json": `[{"ID":21,"Timestamp":"2021-07-14 18:00:00","Contents":"hello all","Attachments":""}]`,
		"c300/channel.json":  `{"id":"300","type":"GUILD_TEXT"}`,
		"c300/messages.json": `[{"ID":31,"Timestamp":"2021-07-14 18:00:00","Contents":"server talk","Attachments":""}]`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadKeepsDirectMessagesAndNamesAttachmentsByID(t *testing.T) {
	channels, leftOut, err := Read(writePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || leftOut != 1 {
		t.Fatalf("read %d channels, left out %d; want 2 and 1", len(channels), leftOut)
	}
	attachment := channels[0].Messages[1].Attachments[0]
	if attachment != (Attachment{ID: "555", Name: "cat.png"}) {
		t.Errorf("attachment = %+v", attachment)
	}
	names := AuthorNames(channels, map[string]string{"3": "Bob"})
	if got := Title(channels[0], "9", names); got != "carol_c" {
		t.Errorf("DM title = %q", got)
	}
	if got := Title(channels[1], "9", names); got != "group with Bob" {
		t.Errorf("group title = %q", got)
	}
}

func TestFullHistoryGivesBothSidesAndKeepsMessagesTheFetchLacks(t *testing.T) {
	channels, _, err := Read(writePackage(t))
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	fetched := `{"id":"13","type":19,"timestamp":"2021-07-10T16:00:00+00:00","edited_timestamp":"2021-07-10T16:01:00+00:00","content":"reply from carol","author":{"id":"1","username":"carol_c","global_name":"Carol"},"attachments":[{"id":"777","filename":"dog.jpg","content_type":"image/jpeg"}],"message_reference":{"message_id":"11"}}
{"id":"11","type":0,"timestamp":"2021-07-10T15:00:00.123+00:00","content":"hi","author":{"id":"9","username":"me"},"attachments":[]}
{"id":"14","type":3,"timestamp":"2021-07-10T17:00:00+00:00","content":"","author":{"id":"1","username":"carol_c"},"attachments":[],"call":{"ended_timestamp":"2021-07-10T17:02:30+00:00"}}
`
	if err := os.WriteFile(filepath.Join(folder, "100.jsonl"), []byte(fetched), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "100.done"), []byte("2026-10-26T19:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unfinished fetch is not used.
	if err := os.WriteFile(filepath.Join(folder, "200.jsonl"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	histories, fetchedAt, err := ReadFullHistory(folder)
	if err != nil {
		t.Fatal(err)
	}
	if merged := MergeFullHistory(channels, histories, fetchedAt); merged != 1 {
		t.Fatalf("merged %d channels, want 1", merged)
	}
	byID := map[string]Message{}
	for _, message := range channels[0].Messages {
		byID[message.ID] = message
	}
	if len(byID) != 4 {
		t.Fatalf("channel 100 has %d messages, want the fetch's 3 and the package's 12", len(byID))
	}
	reply := byID["13"]
	if reply.ReplyToID != "11" || !reply.Edited || reply.AuthorID != "1" || reply.Attachments[0].ID != "777" {
		t.Errorf("reply = %+v", reply)
	}
	if call := byID["14"]; !call.IsCall || call.CallSeconds != 150 || call.Event != "call" {
		t.Errorf("call = %+v", call)
	}
	if channels[1].FullHistoryFetchedAt.IsZero() == false {
		t.Error("the group DM, never fetched, claims a full history")
	}
}
