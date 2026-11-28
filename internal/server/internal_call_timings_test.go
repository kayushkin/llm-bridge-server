package server

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"

	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/references"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repos"
)

// The five busiest calls this server makes to the stores inside it, timed
// with Go's benchmark runner. They need a Postgres database each, so they run
// only when asked:
//
//	go test ./internal/server -run TestInternalCallTimings -benchmark-internal-calls -v
//
// Each prints the benchmark line: iterations, time per call, bytes and
// allocations per call. Every cache in front of a call is emptied before
// each iteration, so the time is the call itself.
var benchmarkInternalCalls = flag.Bool("benchmark-internal-calls", false, "time the busiest calls between this server and the stores inside it")

func reportInternalCallTiming(t *testing.T, call func(b *testing.B)) {
	t.Helper()
	result := testing.Benchmark(call)
	if result.N == 0 {
		t.Fatal("the benchmark failed; run it alone to see why")
	}
	t.Logf("%s\t%s", result.String(), result.MemString())
}

func TestInternalCallTimings(t *testing.T) {
	if !*benchmarkInternalCalls {
		t.Skip("pass -benchmark-internal-calls to time the internal calls")
	}
	ctx := context.Background()

	t.Run("one-shot router reads usage-store limits", func(t *testing.T) {
		gated := newGatedTestServer(t, nil)
		store := enableUsageStoreOnTestDatabase(t, gated.server)
		saveUsageSnapshot(t, store, "anthropic", "seven_day", 40)
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				// A new router keeps no answer from the last iteration.
				if exhausted, err := gated.server.oneShot.NewOneShotRouter().Limits.ExhaustedWindow(ctx, "anthropic"); err != nil || exhausted != "" {
					b.Fatalf("ExhaustedWindow = %q, %v", exhausted, err)
				}
			}
		})
	})

	t.Run("kanban card tags read at spawn", func(t *testing.T) {
		test := newKanbanTestServer(t)
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if tags, err := test.server.issueReads.CardTags(ctx, test.unseenCardID); err != nil || len(tags) != 1 {
					b.Fatalf("CardTags = %v, %v", tags, err)
				}
			}
		})
	})

	t.Run("reference resolve", func(t *testing.T) {
		gated := newGatedTestServer(t, nil)
		repoStore := enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
		if _, err := repoStore.UpsertRepo(&repos.Repo{Name: "webapp", GitRemote: "git@github.com:example-owner/webapp.git"}); err != nil {
			t.Fatal(err)
		}
		resolver := gated.server.referenceResolver.Load()
		// Every answer expires as it is kept, so each resolve asks repo-store.
		resolver.RecordTTL, resolver.MissTTL = 0, 0
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				results, lookupErrors := resolver.Resolve(ctx, references.Caller{Unchecked: true}, []string{"repo:webapp"})
				if len(lookupErrors) > 0 || len(results["repo:webapp"]) != 1 {
					b.Fatalf("Resolve = %+v, %+v", results, lookupErrors)
				}
			}
		})
	})

	t.Run("file-store read of a session file", func(t *testing.T) {
		gated := newGatedTestServer(t, nil)
		enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
		content := strings.Repeat("a session file's bytes\n", 2800) // about 64 KB
		stored, err := gated.server.fileStore.Accept(strings.NewReader(content), int64(len(content)), "text/plain",
			files.NewFile{Filename: "notes.txt", OwnerService: sessionfiles.SessionFileOwnerService, OwnerRef: "br_timed"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				response := &countingResponseWriter{header: http.Header{}}
				gated.server.fileStore.ServeContentOf(response, request, stored.ID, "true")
				if response.status != http.StatusOK || response.written != int64(len(content)) {
					b.Fatalf("content = %d, %d bytes", response.status, response.written)
				}
			}
		})
	})

	t.Run("chat-link sender lookup", func(t *testing.T) {
		gated := newGatedTestServer(t, nil)
		enableConnectedRecordsOnTestDatabase(t, gated)
		if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/people/persons", map[string]any{"display_name": "Sender",
			"identities": []map[string]any{{"kind": "discord", "value": "sender_handle", "linked_by": "operator"}}}); response.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", response.Code, response.Body)
		}
		senders := chatLinkSendersInPeopleStore{server: gated.server}
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if sender, err := senders.SenderOfAccount(ctx, firstTestPrincipalID, "discord", "sender_handle"); err != nil || sender.PersonID == "" {
					b.Fatalf("SenderOfAccount = %+v, %v", sender, err)
				}
			}
		})
	})
}

// countingResponseWriter keeps only the status and how many bytes were
// written, as io.Discard keeps nothing, so a timing counts the call and not a
// copy into memory.
type countingResponseWriter struct {
	header  http.Header
	status  int
	written int64
}

func (writer *countingResponseWriter) Header() http.Header { return writer.header }

func (writer *countingResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *countingResponseWriter) Write(data []byte) (int, error) {
	writer.WriteHeader(http.StatusOK)
	writer.written += int64(len(data))
	return len(data), nil
}
