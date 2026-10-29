package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// fakePeerServer answers the routes bridge-peer calls and records what it got.
type fakePeerServer struct {
	mutex    sync.Mutex
	uploads  map[string][]byte
	sent     []msg.PeerMessageCreateRequest
	serveRaw []byte // what a download answers, to test the SHA-256 check
}

func newFakePeerServer(t *testing.T) (*fakePeerServer, *httptest.Server) {
	fake := &fakePeerServer{uploads: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer posting-token" {
			http.Error(w, "no credential", http.StatusUnauthorized)
			return
		}
		fake.mutex.Lock()
		defer fake.mutex.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/peer-connections":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"connection": map[string]any{"id": "peer_connection_000001", "transport": "dial_out"}, "other_server_name": "main", "socket_connected": true}})
		case r.Method == "POST" && r.URL.Path == "/peer-message-attachments":
			body, _ := io.ReadAll(r.Body)
			id := "peer_attachment_00000" + string(rune('1'+len(fake.uploads)))
			fake.uploads[id] = body
			sum := sha256.Sum256(body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(msg.PeerMessageAttachment{ID: id, Name: r.URL.Query().Get("name"), Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), State: msg.PeerMessageAttachmentStored})
		case r.Method == "POST" && r.URL.Path == "/peer-connections/peer_connection_000001/messages":
			var request msg.PeerMessageCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fake.sent = append(fake.sent, request)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(msg.PeerMessage{ID: "peer_message_000001", Kind: request.Kind, State: msg.PeerMessageStateDelivered})
		case r.Method == "GET" && r.URL.Path == "/peer-messages/peer_message_000002":
			sum := sha256.Sum256([]byte("the real bytes"))
			_ = json.NewEncoder(w).Encode(msg.PeerMessage{ID: "peer_message_000002", Attachments: []msg.PeerMessageAttachment{
				{ID: "peer_attachment_000009", Name: "../escape.patch", Size: int64(len("the real bytes")), SHA256: hex.EncodeToString(sum[:]), State: msg.PeerMessageAttachmentFetched}}})
		case r.Method == "GET" && r.URL.Path == "/peer-messages/peer_message_000002/attachments/peer_attachment_000009":
			_, _ = w.Write(fake.serveRaw)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func runCLI(t *testing.T, server *httptest.Server, arguments ...string) (string, error) {
	t.Helper()
	t.Setenv(postingTokenEnvironmentVariable, "posting-token")
	t.Setenv(serviceTokenEnvironmentVariable, "")
	t.Setenv(actionsURLEnvironmentVariable, server.URL+"/sessions/br_1/actions")
	var out bytes.Buffer
	err := run(arguments, &out)
	return out.String(), err
}

func git(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE="+time.Unix(1700000000, 0).Format(time.RFC3339), "GIT_COMMITTER_DATE="+time.Unix(1700000000, 0).Format(time.RFC3339))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestPatchSendsTheCommitsAfterItsBaseAsOneAttachedFile(t *testing.T) {
	fake, server := newFakePeerServer(t)
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "Start")
	base := git(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-q", "-am", "Change a")
	t.Chdir(repo)

	out, err := runCLI(t, server, "patch", "main", "--repo", "example-repo", "--base", base)
	if err != nil {
		t.Fatalf("patch: %v\n%s", err, out)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(fake.sent))
	}
	sent := fake.sent[0]
	if sent.Kind != msg.PeerMessageKindPatch || sent.Body.Repo != "example-repo" || sent.Body.BaseCommit != base || sent.Body.Summary != "Change a" || len(sent.AttachmentIDs) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	uploaded := string(fake.uploads[sent.AttachmentIDs[0]])
	if !strings.Contains(uploaded, "Subject: [PATCH] Change a") || !strings.Contains(uploaded, "+two") {
		t.Fatalf("the attached file is not the format-patch of the commit after the base:\n%s", uploaded)
	}

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, server, "patch", "main", "--repo", "example-repo", "--base", base); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("patch with uncommitted changes = %v, want a refusal", err)
	}
	git(t, repo, "checkout", "-q", "--", "a.txt")
	if _, err := runCLI(t, server, "patch", "main", "--repo", "example-repo", "--base", "HEAD"); err == nil || !strings.Contains(err.Error(), "nothing to send") {
		t.Fatalf("patch with no commits after the base = %v, want a refusal", err)
	}
}

func TestRequestAttachesEachFileItIsGiven(t *testing.T) {
	fake, server := newFakePeerServer(t)
	file := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(file, []byte("some notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, server, "request", "main", "--title", "Look at this", "--attach", file, "please read the notes"); err != nil {
		t.Fatalf("request: %v\n%s", err, out)
	}
	if len(fake.sent) != 1 || len(fake.sent[0].AttachmentIDs) != 1 || string(fake.uploads[fake.sent[0].AttachmentIDs[0]]) != "some notes" {
		t.Fatalf("sent %+v, uploads %v", fake.sent, fake.uploads)
	}
}

func TestFetchKeepsOnlyAFileWhoseSHA256Matches(t *testing.T) {
	fake, server := newFakePeerServer(t)
	directory := t.TempDir()
	fake.serveRaw = []byte("tampered bytes")
	if _, err := runCLI(t, server, "fetch", "peer_message_000002", "--out", directory); err == nil || !strings.Contains(err.Error(), "removed it") {
		t.Fatalf("fetch of a mismatching file = %v, want a refusal", err)
	}
	if entries, _ := os.ReadDir(directory); len(entries) != 0 {
		t.Fatalf("a mismatching file was left in %s: %v", directory, entries)
	}
	fake.serveRaw = []byte("the real bytes")
	if out, err := runCLI(t, server, "fetch", "peer_message_000002", "--out", directory); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	// The sender's name is cut to its base, so it cannot write outside the directory.
	data, err := os.ReadFile(filepath.Join(directory, "escape.patch"))
	if err != nil || string(data) != "the real bytes" {
		t.Fatalf("fetched file = %q, %v", data, err)
	}
}
