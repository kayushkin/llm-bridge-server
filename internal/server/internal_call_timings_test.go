package server

import (
	"context"
	"flag"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
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
				if exhausted, err := gated.server.newOneShotRouter().Limits.ExhaustedWindow(ctx, "anthropic"); err != nil || exhausted != "" {
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
				if tags, err := test.server.kanbanClient.CardTags(ctx, test.unseenCardID); err != nil || len(tags) != 1 {
					b.Fatalf("CardTags = %v, %v", tags, err)
				}
			}
		})
	})

	t.Run("reference resolve", func(t *testing.T) {
		gated := newGatedTestServer(t, nil)
		repos := enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
		if _, err := repos.UpsertRepo(&repostore.Repo{Name: "webapp", GitRemote: "git@github.com:example-owner/webapp.git"}); err != nil {
			t.Fatal(err)
		}
		resolver := gated.server.referenceResolver.Load()
		// Every answer expires as it is kept, so each resolve asks repo-store.
		resolver.RecordTTL, resolver.MissTTL = 0, 0
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				results, lookupErrors := resolver.Resolve(ctx, referencestore.Caller{Unchecked: true}, []string{"repo:webapp"})
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
		stored, err := gated.server.fileStore.Upload(strings.NewReader(content), int64(len(content)), "text/plain", "notes.txt", "br_timed")
		if err != nil {
			t.Fatal(err)
		}
		reportInternalCallTiming(t, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				response, err := gated.server.fileStore.Content(stored.ID, true, "")
				if err != nil {
					b.Fatal(err)
				}
				read, err := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || read != int64(len(content)) {
					b.Fatalf("content = %d, %d bytes, %v", response.StatusCode, read, err)
				}
			}
		})
	})

	t.Run("chat-link sender lookup", func(t *testing.T) {
		gated := newGatedTestServer(t, nil)
		enableConnectedRecordsOnTestDatabase(t, gated)
		if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/people-store/persons", map[string]any{"display_name": "Sender",
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
