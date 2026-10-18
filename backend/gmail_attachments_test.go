package backend

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"testing"
)

// The shape of a real Samsung Notes export mailed from a phone: a short body
// in multipart/alternative, then each note as a text/plain part with a file
// name. Before the fix the attached parts were read as the body, so the
// message showed the last attached file's (empty) inline data as its text and
// listed no attachments.
const gmailMessageWithTextAttachments = `{
	"id":"m1",
	"payload":{"partId":"","mimeType":"multipart/mixed","parts":[
		{"partId":"0","mimeType":"multipart/alternative","parts":[
			{"partId":"0.0","mimeType":"text/plain","body":{"data":"` + "Tm90ZXMK" + `"}},
			{"partId":"0.1","mimeType":"text/html","body":{"data":"PGRpdj5Ob3RlczwvZGl2Pg"}}
		]},
		{"partId":"1","mimeType":"text/plain","filename":"Notes_260921_094127.txt","body":{"size":11,"attachmentId":"ANGjdJ-first"}},
		{"partId":"2","mimeType":"text/plain","filename":"Costco _260113_194226.txt","body":{"size":4,"data":"bWlsaw"}}
	]}}`

func TestGmailGetListsAttachedTextFilesAsAttachmentsAndKeepsTheBody(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages/m1", http.StatusOK, gmailMessageWithTextAttachments)

	msg, err := newRecordedGmail(t, api, &fakeTokenSource{}).Get(context.Background(), "", "m1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if msg.BodyText != "Notes\n" || msg.BodyHTML != "<div>Notes</div>" {
		t.Errorf("an attached text file must not replace the body, got text=%q html=%q", msg.BodyText, msg.BodyHTML)
	}
	want := []Attachment{
		{ID: "1", Filename: "Notes_260921_094127.txt", MimeType: "text/plain", Size: 11},
		{ID: "2", Filename: "Costco _260113_194226.txt", MimeType: "text/plain", Size: 4},
	}
	if len(msg.Attachments) != len(want) {
		t.Fatalf("want %d attachments, got %+v", len(want), msg.Attachments)
	}
	for i := range want {
		if msg.Attachments[i] != want[i] {
			t.Errorf("attachment %d: want %+v, got %+v", i, want[i], msg.Attachments[i])
		}
	}
	if !msg.Meta.HasAttach {
		t.Error("a message with attachments must say so in its meta")
	}
}

// Gmail hands out a fresh attachmentId on every messages.get, so Attachment
// takes the stable partId, reads the message again, and fetches by the
// attachmentId that read returned.
func TestGmailAttachmentFetchesByTheAttachmentIdOfAFreshRead(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages/m1", http.StatusOK, gmailMessageWithTextAttachments)
	api.route("/users/me/messages/m1/attachments/ANGjdJ-first", http.StatusOK,
		`{"size":11,"data":"`+base64.RawURLEncoding.EncodeToString([]byte("first note"))+`"}`)

	body, contentType, err := newRecordedGmail(t, api, &fakeTokenSource{}).Attachment(context.Background(), "m1", "1")
	if err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "first note" || contentType != "text/plain" {
		t.Errorf("want %q as text/plain, got %q as %q", "first note", data, contentType)
	}
}

func TestGmailAttachmentAnswersAnInlinePartFromTheMessageItself(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages/m1", http.StatusOK, gmailMessageWithTextAttachments)

	body, _, err := newRecordedGmail(t, api, &fakeTokenSource{}).Attachment(context.Background(), "m1", "2")
	if err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "milk" {
		t.Errorf("want the inline bytes %q, got %q", "milk", data)
	}
	if n := len(api.recorded()); n != 1 {
		t.Errorf("an inline part needs only the message read, got %d requests", n)
	}
}

// A body part has a partId too, but it is not an attachment, and asking for
// one must say so rather than hand back the body.
func TestGmailAttachmentRefusesAPartThatIsNotAnAttachment(t *testing.T) {
	for _, partID := range []string{"0.0", "9"} {
		api := newRecordingAPI(t)
		api.route("/users/me/messages/m1", http.StatusOK, gmailMessageWithTextAttachments)

		_, _, err := newRecordedGmail(t, api, &fakeTokenSource{}).Attachment(context.Background(), "m1", partID)
		if !errors.Is(err, ErrAttachmentNotFound) {
			t.Errorf("part %q: want ErrAttachmentNotFound, got %v", partID, err)
		}
	}
}
