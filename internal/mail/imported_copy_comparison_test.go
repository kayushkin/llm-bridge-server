package mail

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/localmail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailmodule"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailmodule/mailmoduletest"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// The landing-day check of the import, against a copy of a live store:
//
//	go test -run TestImportedCopyAnswersAsTheStandaloneStoreDid . -args \
//	  -standalone-accounts-copy=<accounts.db copy> -standalone-local-mail-copy=<local-mail.db copy> \
//	  -standalone-url=<the standalone mailstack, serving those same copies, with no token>
//
// It imports both copies into a database of its own, then asks the moved
// store and the standalone one the same read-only questions, on routes that
// never reach a mail provider, and compares the answers. Search ranks with
// BM25 on both sides, computed by FTS5 there and in SQL here, so search is
// compared by which messages come back and in what order. It prints counts
// and agreement only, never a message: the copy is someone's mail.
var (
	standaloneAccountsCopy  = flag.String("standalone-accounts-copy", "", "a copy of a standalone mailstack's accounts.db")
	standaloneLocalMailCopy = flag.String("standalone-local-mail-copy", "", "a copy of its local-mail.db")
	standaloneURL           = flag.String("standalone-url", "", "the standalone mailstack serving those copies")
)

func TestImportedCopyAnswersAsTheStandaloneStoreDid(t *testing.T) {
	if *standaloneAccountsCopy == "" || *standaloneURL == "" {
		t.Skip("pass -standalone-accounts-copy, -standalone-local-mail-copy and -standalone-url to compare an import with a live store's copy")
	}
	ctx := context.Background()
	db := mailmoduletest.NewDB(t)
	counts, err := mailmodule.ImportStandaloneDatabases(ctx, db.Pool(), *standaloneAccountsCopy, *standaloneLocalMailCopy)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	encoded, _ := json.Marshal(counts)
	t.Logf("imported: %s", encoded)

	sources := map[string]string{"accounts": *standaloneAccountsCopy, "local mail": *standaloneLocalMailCopy}
	compareTablesRowByRow(t, ctx, db.Pool(), sources)

	moved := newTestServer(t, accounts.New(db), localmail.New(db), ServerParts{})
	movedServer := httptest.NewServer(testCallerHandler(moved))
	t.Cleanup(movedServer.Close)

	source, err := standalonesqlite.Open(*standaloneAccountsCopy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	paths := []string{"/health", "/api/accounts", "/api/imap/creds"}
	var localAccounts []string
	rows, err := source.QueryContext(ctx, "SELECT id FROM accounts WHERE provider = 'local' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		localAccounts = append(localAccounts, id)
	}
	rows.Close()
	for _, account := range localAccounts {
		paths = append(paths, "/api/folders?account="+url.QueryEscape(account), "/api/accounts/"+url.PathEscape(account)+"/import-sources")
		cursor := ""
		for page := 0; page < 100; page++ {
			path := "/api/messages?account=" + url.QueryEscape(account) + "&limit=200"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			paths = append(paths, path)
			var listed struct {
				Messages []struct {
					ID string `json:"id"`
				} `json:"messages"`
				NextCursor string `json:"next_cursor"`
			}
			_, listing, _ := bytes.Cut(get(t, *standaloneURL+path), []byte(" "))
			if err := json.Unmarshal(listing, &listed); err != nil {
				t.Fatalf("standalone %s: %v", redactPath(path), err)
			}
			for index, message := range listed.Messages {
				if index%10 == 0 {
					base := "/api/messages/" + url.PathEscape(message.ID)
					query := "?account=" + url.QueryEscape(account)
					paths = append(paths, base+query, base+"/metadata"+query, base+"/body"+query+"&format=text",
						base+"/body"+query+"&format=html", base+"/revisions"+query)
				}
			}
			if listed.NextCursor == "" {
				break
			}
			cursor = listed.NextCursor
		}
	}
	// Message-ID lookups and threads, for every 50th indexed message.
	indexed, err := source.QueryContext(ctx, `SELECT account_id, folder_id, provider_id, message_id FROM messages ORDER BY account_id, folder_scope, provider_id`)
	if err != nil {
		t.Fatal(err)
	}
	for row := 0; indexed.Next(); row++ {
		var account, folder, provider, messageID string
		indexed.Scan(&account, &folder, &provider, &messageID)
		if row%50 != 0 {
			continue
		}
		if messageID != "" {
			paths = append(paths, "/api/lookup/message-id/"+url.PathEscape(messageID))
		}
		paths = append(paths, "/api/messages/"+url.PathEscape(provider)+"/thread?account="+url.QueryEscape(account)+"&folder="+url.QueryEscape(folder))
	}
	indexed.Close()
	// Last: a local account's listing records what it lists in the index, on
	// both sides, so the counts compare only once both have listed it.
	paths = append(paths, "/api/index/stats")

	same, differ := 0, 0
	for _, path := range paths {
		standalone, moved := get(t, *standaloneURL+path), get(t, movedServer.URL+path)
		if equalJSON(standalone, moved) {
			same++
			continue
		}
		differ++
		if differ <= 10 {
			t.Errorf("GET %s: the answers differ (%d and %d bytes)", redactPath(path), len(standalone), len(moved))
		}
	}
	t.Logf("GET answers: %d identical, %d different, of %d", same, differ, len(paths))

	compareSearch(t, ctx, source, *standaloneURL, movedServer.URL)
}

// compareTablesRowByRow reads every table the import copied from both sides,
// in key order, and counts the rows whose values differ.
func compareTablesRowByRow(t *testing.T, ctx context.Context, pool *sql.DB, sources map[string]string) {
	t.Helper()
	tables := map[string][][2]string{
		"accounts": {{"accounts", "id"}, {"imap_creds", "name"}, {"imap_folder_map", "account_id, canonical"},
			{"messages", "account_id, folder_scope, provider_id"},
			{"message_bodies", "id"}},
		"local mail": {{"local_messages", "account_id, id"}, {"local_message_bodies", "account_id, id"},
			{"local_import_sources", "account_id, source_ref"}, {"local_message_revisions", "account_id, id, revision"},
			{"local_message_attachments", "account_id, id, revision, attachment_id"}, {"ingress_credentials", "credential_id"},
			{"ingress_credential_accounts", "credential_id, account_id"}, {"delivery_receipts", "receipt_id"},
			{"message_purges", "account_id, provider_message_id"}, {"message_events", "seq"}},
	}
	for what, list := range tables {
		if sources[what] == "" {
			continue
		}
		source, err := standalonesqlite.Open(sources[what])
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range list {
			name, key := table[0], table[1]
			columns := sqliteColumns(t, ctx, source, name)
			orderBy := keyOrder(key)
			want := readAll(t, ctx, source, "SELECT "+strings.Join(columns, ", ")+" FROM "+name+" ORDER BY "+key)
			got := readAll(t, ctx, pool, "SELECT "+strings.Join(columns, ", ")+" FROM "+name+" ORDER BY "+orderBy)
			differing := 0
			for index := range want {
				if index >= len(got) || !reflect.DeepEqual(normalized(want[index]), normalized(got[index])) {
					differing++
				}
			}
			if len(want) != len(got) || differing > 0 {
				t.Errorf("%s: %d rows in the copy, %d imported, %d differ", name, len(want), len(got), differing)
			} else {
				t.Logf("%s: %d rows, every one identical", name, len(want))
			}
		}
		source.Close()
	}
}

func keyOrder(key string) string {
	parts := strings.Split(key, ", ")
	for index, part := range parts {
		parts[index] = part + ` COLLATE "C"`
		if part == "id" || part == "seq" || part == "receipt_id" || part == "revision" {
			parts[index] = part
		}
	}
	return strings.Join(parts, ", ")
}

func sqliteColumns(t *testing.T, ctx context.Context, source *sql.DB, table string) []string {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		columns = append(columns, name)
	}
	return columns
}

func readAll(t *testing.T, ctx context.Context, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		out = append(out, values)
	}
	return out
}

// normalized reads every value as SQLite and Postgres both can hand it back:
// numbers as int64, text as string, as Postgres holds it.
func normalized(row []any) []any {
	out := make([]any, len(row))
	for index, value := range row {
		switch typed := value.(type) {
		case []byte:
			out[index], _ = mailmodule.TextPostgresCanHold(string(typed))
		case string:
			out[index], _ = mailmodule.TextPostgresCanHold(typed)
		case int32:
			out[index] = int64(typed)
		case int:
			out[index] = int64(typed)
		default:
			out[index] = value
		}
	}
	return out
}

func get(t *testing.T, target string) []byte {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", redactPath(target), err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return append([]byte(fmt.Sprintf("%d ", response.StatusCode)), body...)
}

func equalJSON(a, b []byte) bool {
	statusA, bodyA, _ := bytes.Cut(a, []byte(" "))
	statusB, bodyB, _ := bytes.Cut(b, []byte(" "))
	if !bytes.Equal(statusA, statusB) {
		return false
	}
	var valueA, valueB any
	if json.Unmarshal(bodyA, &valueA) != nil || json.Unmarshal(bodyB, &valueB) != nil {
		return bytes.Equal(bodyA, bodyB)
	}
	return reflect.DeepEqual(withoutLastSeen(valueA), withoutLastSeen(valueB))
}

// withoutLastSeen drops last_seen_ms, which each side sets to the moment it
// last listed a message: the two stores list the same mail a moment apart.
func withoutLastSeen(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, inner := range typed {
			if key != "last_seen_ms" {
				out[key] = withoutLastSeen(inner)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, inner := range typed {
			out[index] = withoutLastSeen(inner)
		}
		return out
	}
	return value
}

// redactPath keeps a route's shape and drops its ids, which can be
// addresses.
func redactPath(path string) string {
	if index := strings.Index(path, "?"); index >= 0 {
		path = path[:index]
	}
	parts := strings.Split(path, "/")
	for index, part := range parts {
		if index >= 3 && part != "metadata" && part != "body" && part != "thread" && part != "revisions" {
			parts[index] = "…"
		}
	}
	return strings.Join(parts, "/")
}

// compareSearch asks both stores the same searches and similarity questions:
// single words drawn from recorded subjects, and whole recorded messages as
// the classifier sends them, each excluding itself.
func compareSearch(t *testing.T, ctx context.Context, source *sql.DB, standalone, moved string) {
	t.Helper()
	rows, err := source.QueryContext(ctx, `SELECT account_id, provider_id, subject, from_email, search_text FROM message_bodies ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type sample struct{ account, provider, subject, from, text string }
	var samples []sample
	for row := 0; rows.Next(); row++ {
		var s sample
		rows.Scan(&s.account, &s.provider, &s.subject, &s.from, &s.text)
		if row%100 == 0 {
			samples = append(samples, s)
		}
	}
	rows.Close()

	type hits struct {
		Hits []struct {
			AccountID  string `json:"account_id"`
			ProviderID string `json:"provider_id"`
		} `json:"hits"`
	}
	durations := map[string][]time.Duration{}
	ranked := func(base, method, path string, body []byte) []string {
		started := time.Now()
		defer func() {
			side := "moved"
			if base == standalone {
				side = "standalone"
			}
			durations[side+" "+method] = append(durations[side+" "+method], time.Since(started))
		}()
		var response *http.Response
		var err error
		if method == http.MethodPost {
			response, err = http.Post(base+path, "application/json", bytes.NewReader(body))
		} else {
			response, err = http.Get(base + path)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var answer hits
		json.NewDecoder(response.Body).Decode(&answer)
		out := make([]string, 0, len(answer.Hits))
		for _, hit := range answer.Hits {
			out = append(out, hit.AccountID+":"+hit.ProviderID)
		}
		return out
	}
	var searches, similars agreement
	for _, s := range samples {
		words := tokenizeWordsForComparison(s.subject)
		if len(words) > 0 {
			path := "/api/search?limit=10&q=" + url.QueryEscape(words[0])
			searches.add(ranked(standalone, http.MethodGet, path, nil), ranked(moved, http.MethodGet, path, nil))
		}
		query := s.subject + "\n" + s.from + "\n" + s.text
		body, _ := json.Marshal(map[string]any{"text": query, "limit": 20,
			"exclude": map[string]string{"account_id": s.account, "provider_id": s.provider}})
		similars.add(ranked(standalone, http.MethodPost, "/api/search/similar", body), ranked(moved, http.MethodPost, "/api/search/similar", body))
	}
	t.Logf("one-word searches: %s", searches)
	t.Logf("similarity queries: %s", similars)
	for side, list := range durations {
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		t.Logf("%s (GET is search, POST similarity): median %s, slowest %s", side, list[len(list)/2].Round(time.Millisecond), list[len(list)-1].Round(time.Millisecond))
	}
}

func tokenizeWordsForComparison(text string) []string {
	var words []string
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(word) >= 4 {
			words = append(words, word)
		}
	}
	return words
}

// agreement tallies how alike two ranked lists are.
type agreement struct {
	queries, sameFirst, sameSet, sameOrder int
	overlapAt5, overlapAt10                []float64
}

func (a *agreement) add(want, got []string) {
	a.queries++
	if len(want) > 0 && len(got) > 0 && want[0] == got[0] || len(want) == 0 && len(got) == 0 {
		a.sameFirst++
	}
	if reflect.DeepEqual(want, got) {
		a.sameOrder++
	}
	sortedWant, sortedGot := append([]string{}, want...), append([]string{}, got...)
	sort.Strings(sortedWant)
	sort.Strings(sortedGot)
	if reflect.DeepEqual(sortedWant, sortedGot) {
		a.sameSet++
	}
	a.overlapAt5 = append(a.overlapAt5, overlap(want, got, 5))
	a.overlapAt10 = append(a.overlapAt10, overlap(want, got, 10))
}

func overlap(want, got []string, depth int) float64 {
	if len(want) > depth {
		want = want[:depth]
	}
	if len(got) > depth {
		got = got[:depth]
	}
	if len(want) == 0 {
		if len(got) == 0 {
			return 1
		}
		return 0
	}
	in := map[string]bool{}
	for _, id := range got {
		in[id] = true
	}
	shared := 0
	for _, id := range want {
		if in[id] {
			shared++
		}
	}
	return float64(shared) / float64(len(want))
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func (a agreement) String() string {
	return fmt.Sprintf("%d queries; same first hit %d, same set %d, same order %d; mean overlap of the top 5 %.2f, of the top 10 %.2f",
		a.queries, a.sameFirst, a.sameSet, a.sameOrder, mean(a.overlapAt5), mean(a.overlapAt10))
}
