package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/photo-store/internal/importbatch"
)

func TestAPictureFromADeletedMessageIsImportedLikeAnyOther(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/message-log":
			json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{{
				"ref": "message_000042", "media_url": "mxc://chat.example/abc", "media_mimetype": "image/png",
				"sent_at": "2026-10-01T12:00:00Z", "redacted_at": "2026-10-01T12:05:00Z",
			}}})
		case "/api/media/chat.example/abc":
			w.Write(picture.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	in := importbatch.New()
	chat := multichat{base: server.URL, token: "t", client: server.Client()}
	if err := addRoomImages(chat, in, "!room:chat.example", func(loggedMessage) string { return "folder" }); err != nil {
		t.Fatal(err)
	}
	if len(in.Photos) != 1 || in.Photos[0].SourcePath != "message_000042" {
		t.Fatalf("photos = %+v, report = %v", in.Photos, in.Report)
	}
}
