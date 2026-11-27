package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/backend"
	"github.com/kayushkin/llm-bridge-server/internal/mail/localmail"
)

// The latency check behind the ticket desk's targets. It is off by default
// because it writes a few hundred megabytes into the test Postgres and takes a
// minute or two; the shared test container is small, so give it one of its own:
//
//	go test -run TestSplitReadLatency -split-read-latency -v .
//
// It fixes one ticket — 25 linked messages, one with a 250 KB HTML body — and
// reads it through real loopback HTTP, as the internal service, while the
// mailbox around it grows. Reading a ticket's messages has to cost the same
// whatever the size of the mailbox, so the test fails if the largest mailbox is
// more than twice as slow as the smallest, as well as if a p95 misses the
// target.
var splitReadLatency = flag.Bool("split-read-latency", false, "run the split-read latency check against growing mailboxes")

const (
	latencyTicketMessages = 25
	latencyIterations     = 300
	latencyConcurrency    = 8
	latencyLargeHTMLBytes = 250 << 10
	latencyResolveTarget  = 100 * time.Millisecond
	latencyBodyTarget     = 150 * time.Millisecond
)

func TestSplitReadLatency(t *testing.T) {
	if !*splitReadLatency {
		t.Skip("pass -split-read-latency to run")
	}
	accountsStore, store := newTestStores(t)
	if err := accountsStore.Save(&accounts.Account{ID: "desk", Label: "Desk", Provider: accounts.ProviderLocal,
		Address: "support@desk.example", Enabled: true}); err != nil {
		t.Fatalf("save account: %v", err)
	}

	// The ticket's own messages go in first, so every later mailbox size reads
	// exactly the same rows.
	ticket := make([]localmail.ImportedMessage, latencyTicketMessages)
	for i := range ticket {
		ticket[i] = latencyMessage(fmt.Sprintf("ticket-%02d", i), 4<<10, 16<<10)
	}
	ticket[0] = latencyMessage("ticket-00", 8<<10, latencyLargeHTMLBytes)
	outcomes, _, _, err := store.Import(context.Background(), "desk", ticket, nil)
	if err != nil {
		t.Fatalf("import ticket: %v", err)
	}
	refs := make([]resolveRef, len(outcomes))
	for i, o := range outcomes {
		refs[i] = resolveRef{Locator: o.Locator}
	}
	resolveBody, _ := json.Marshal(map[string]any{"refs": refs})
	largeID := outcomes[0].ID

	type row struct {
		mailbox                       int
		importRate                    float64
		resolveP50, resolveP95        time.Duration
		bodyP50, bodyP95              time.Duration
		concurrentP95, firstAfterOpen time.Duration
	}
	var rows []row
	filled := latencyTicketMessages
	for _, size := range []int{1_000, 10_000, 50_000} {
		started := time.Now()
		added := 0
		for filled < size {
			batch := make([]localmail.ImportedMessage, 0, localmail.MaxImportBatch)
			for len(batch) < localmail.MaxImportBatch && filled < size {
				batch = append(batch, latencyMessage(fmt.Sprintf("filler-%07d", filled), 2<<10, 6<<10))
				filled++
			}
			if _, _, _, err := store.Import(context.Background(), "desk", batch, nil); err != nil {
				t.Fatalf("import filler: %v", err)
			}
			added += len(batch)
		}
		importRate := float64(added) / time.Since(started).Seconds()

		// A fresh server on the same module, as after a restart.
		server := newTestServer(t, accountsStore, store, ServerParts{})
		listener := httptest.NewServer(testCallerHandler(server))
		client := listener.Client()
		call := func(method, path string, body []byte) time.Duration {
			req, _ := http.NewRequest(method, listener.URL+path, bytes.NewReader(body))
			began := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("%s %s: %v", method, path, err)
				return 0
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s %s: status %d", method, path, resp.StatusCode)
			}
			if strings.Contains(path, "/body") && n < latencyLargeHTMLBytes {
				t.Errorf("body read %d bytes, want the whole %d", n, latencyLargeHTMLBytes)
			}
			return time.Since(began)
		}
		resolve := func() time.Duration { return call(http.MethodPost, "/api/messages/resolve", resolveBody) }
		body := func() time.Duration {
			return call(http.MethodGet, "/api/messages/"+largeID+"/body?account=desk&format=html", nil)
		}

		first := resolve()
		var resolveTimes, bodyTimes []time.Duration
		for i := 0; i < latencyIterations; i++ {
			resolveTimes = append(resolveTimes, resolve())
			bodyTimes = append(bodyTimes, body())
		}
		var mu sync.Mutex
		var concurrent []time.Duration
		var wg sync.WaitGroup
		for worker := 0; worker < latencyConcurrency; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < latencyIterations/latencyConcurrency*2; i++ {
					took := resolve() + body()
					mu.Lock()
					concurrent = append(concurrent, took)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		listener.Close()
		server.Shutdown(context.Background())

		rows = append(rows, row{
			mailbox: size, importRate: importRate, firstAfterOpen: first,
			resolveP50: percentile(resolveTimes, 50), resolveP95: percentile(resolveTimes, 95),
			bodyP50: percentile(bodyTimes, 50), bodyP95: percentile(bodyTimes, 95),
			concurrentP95: percentile(concurrent, 95),
		})
	}

	t.Logf("%9s %12s %12s %12s %12s %12s %16s %14s", "mailbox", "import msg/s", "resolve p50", "resolve p95",
		"250KB p50", "250KB p95", "8-way res+body", "first request")
	for _, r := range rows {
		t.Logf("%9d %12.0f %12s %12s %12s %12s %16s %14s", r.mailbox, r.importRate,
			r.resolveP50.Round(10*time.Microsecond), r.resolveP95.Round(10*time.Microsecond),
			r.bodyP50.Round(10*time.Microsecond), r.bodyP95.Round(10*time.Microsecond),
			r.concurrentP95.Round(10*time.Microsecond), r.firstAfterOpen.Round(10*time.Microsecond))
		if r.resolveP95 > latencyResolveTarget {
			t.Errorf("mailbox %d: resolve of %d refs p95 %s, target %s", r.mailbox, latencyTicketMessages, r.resolveP95, latencyResolveTarget)
		}
		if r.bodyP95 > latencyBodyTarget {
			t.Errorf("mailbox %d: 250 KB body p95 %s, target %s", r.mailbox, r.bodyP95, latencyBodyTarget)
		}
	}
	smallest, largest := rows[0], rows[len(rows)-1]
	if largest.resolveP95 > 2*smallest.resolveP95 || largest.bodyP95 > 2*smallest.bodyP95 {
		t.Errorf("reading one ticket grew with the mailbox: resolve p95 %s → %s, body p95 %s → %s",
			smallest.resolveP95, largest.resolveP95, smallest.bodyP95, largest.bodyP95)
	}
}

func latencyMessage(providerID string, textBytes, htmlBytes int) localmail.ImportedMessage {
	text := strings.Repeat("Bitte prüfen Sie die Lieferung. ", textBytes/33+1)[:textBytes]
	html := "<html><body>" + strings.Repeat("<p>Lieferschein über 1.200 € — 発注書</p>", htmlBytes/46+1)
	html = html[:htmlBytes-len("</body></html>")] + "</body></html>"
	return localmail.ImportedMessage{
		ProviderMessageID: providerID,
		MessageID:         providerID + "@outlook.example",
		ConversationID:    "conv-" + providerID,
		Subject:           "Lieferung " + providerID,
		From:              backend.Address{Name: "Kunde", Email: "kunde@customer.example"},
		To:                []backend.Address{{Email: "support@desk.example"}},
		ReceivedAt:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(providerID)) * time.Minute),
		BodyText:          &text,
		BodyHTML:          &html,
		Outlook:           &localmail.OutlookLocation{StoreID: "S", FolderID: "F", FolderPath: `\\Support\Inbox`},
		Snippet:           localmail.Snippet(text),
	}
}

func percentile(samples []time.Duration, p int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (len(sorted)*p+99)/100 - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}
