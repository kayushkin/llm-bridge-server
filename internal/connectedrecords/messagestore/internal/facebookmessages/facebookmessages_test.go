package facebookmessages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSmallThreadsAreReadRepairedAndKeyedAndBigOnesAreLeftOut(t *testing.T) {
	dir := t.TempDir()
	// "Zoë" and "👍" as the export writes them: each UTF-8 byte as a \u00XX escape.
	write(t, filepath.Join(dir, "inbox", "zoe_111", "message_1.json"), `{"title":"ZoÃ«","participants":[{"name":"ZoÃ«"},{"name":"Me Self"}],
	"messages":[
	 {"sender_name":"ZoÃ«","timestamp_ms":1590000060000,"content":"hey ð\u009f\u0091\u008d"},
	 {"sender_name":"ZoÃ«","timestamp_ms":1590000060000,"content":"hey ð\u009f\u0091\u008d"},
	 {"sender_name":"Me Self","timestamp_ms":1590000000000,"photos":[{"uri":"your_facebook_activity/messages/inbox/zoe_111/photos/cat.jpg"}]},
	 {"sender_name":"ZoÃ«","timestamp_ms":1590000120000,"call_duration":61},
	 {"sender_name":"ZoÃ«","timestamp_ms":1590000180000,"call_duration":0,"missed":true}]}`)
	big := `{"title":"Huge","participants":[` + strings.Repeat(`{"name":"x"},`, 8) + `{"name":"Me Self"}],"messages":[{"sender_name":"x","timestamp_ms":1590000000000,"content":"hi"}]}`
	write(t, filepath.Join(dir, "inbox", "huge_222", "message_1.json"), big)
	threads, skipped, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || skipped.TooManyParticipants != 1 || skipped.Messages != 1 {
		t.Fatalf("threads %d, skipped %+v", len(threads), skipped)
	}
	thread := threads[0]
	if Title(thread, "Me Self") != "Zoë" || thread.Ref() != "inbox/zoe_111" {
		t.Errorf("title %q ref %q", Title(thread, "Me Self"), thread.Ref())
	}
	first, copy := thread.Messages[0], thread.Messages[1]
	if first.Content != "hey 👍" || first.Sender != "Zoë" {
		t.Errorf("first = %+v", first)
	}
	if !strings.HasPrefix(first.Ref, "inbox/zoe_111/1590000060000/") || copy.Ref != first.Ref+"/2" {
		t.Errorf("refs %q and %q", first.Ref, copy.Ref)
	}
	if photo := thread.Messages[2]; len(photo.Files) != 1 || photo.Files[0].Name() != "cat.jpg" || photo.Files[0].Kind != "photo" {
		t.Errorf("photo = %+v", photo)
	}
	if call := thread.Messages[3]; !call.IsCall || call.CallSeconds != 61 {
		t.Errorf("call = %+v", call)
	}
	if missed := thread.Messages[4]; !missed.IsCall || missed.CallSeconds != 0 {
		t.Errorf("missed = %+v", missed)
	}
	again, _, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Messages[1].Ref != copy.Ref {
		t.Error("a second read keys the messages differently")
	}
}
