package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kayushkin/photo-store/internal/importbatch"
	"github.com/kayushkin/photo-store/internal/messagecitation"
)

func TestADirectMessageImageCitesTheStoredMessageItWasSentIn(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/messages" && r.URL.Query().Get("source") == "discord" && r.URL.Query().Get("source_ref") == "1400000000000000001":
			json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "chat_message_000007", "conversation_id": "conversation_000003"}}})
		case r.URL.Path == "/messages":
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
		case r.URL.Path == "/conversations/conversation_000003":
			json.NewEncoder(w).Encode(map[string]any{"id": "conversation_000003", "title": "sam"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer store.Close()

	folder := t.TempDir()
	if err := os.MkdirAll(filepath.Join(folder, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "images", "a.png"), picture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"message_id":"1400000000000000001","content_type":"image/png","path":"images/a.png"}
{"message_id":"1400000000000000002","content_type":"image/png","path":"images/a.png"}
`
	if err := os.WriteFile(filepath.Join(folder, "images.jsonl"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	in := importbatch.New()
	if err := importDirectMessages(messagecitation.Store{Base: store.URL, Client: store.Client()}, in, folder); err != nil {
		t.Fatal(err)
	}
	if len(in.Photos) != 1 {
		t.Fatalf("photos = %+v, report = %v", in.Photos, in.Report)
	}
	if got := in.Photos[0]; got.SourcePath != "chat_message_000007" || got.Folder != "discord/direct-messages/sam" {
		t.Fatalf("photo = %+v", got)
	}
	if in.Report["message-store holds not one message for it (0 messages are filed under it)"] != 1 {
		t.Fatalf("a picture whose message is not stored was not reported: %v", in.Report)
	}
}
