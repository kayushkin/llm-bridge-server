package toolstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func postJSON(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var buffer bytes.Buffer
	buffer.ReadFrom(response.Body)
	return response.StatusCode, buffer.Bytes()
}

func decodeObserved(t *testing.T, body []byte) ObservedHarnessToolsResponse {
	t.Helper()
	var response ObservedHarnessToolsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return response
}

func TestObservedCreatesAnUnknownToolSwitchedOffAndUnreviewed(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{Name: "claude_code.Read", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Read", Enabled: true, Tags: []string{"read-only"}}); err != nil {
		t.Fatal(err)
	}
	status, body := postJSON(t, "POST", srv.URL+"/harness-tools/observed", `{"harness":"claude_code","tool_names":["Read","Frobnicate"]}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	response := decodeObserved(t, body)
	if !reflect.DeepEqual(response.Created, []string{"Frobnicate"}) || response.Seen != 2 {
		t.Fatalf("response %+v", response)
	}
	created, err := s.GetToolByName("claude_code.Frobnicate")
	if err != nil {
		t.Fatal(err)
	}
	if created.Enabled || created.Kind != KindHarness || created.Harness != "claude_code" || created.HarnessToolName != "Frobnicate" {
		t.Fatalf("created row wrong: %+v", created)
	}
	if !reflect.DeepEqual(created.Tags, []string{TagUnreviewed}) || created.LastSeenAt == 0 {
		t.Fatalf("created row tags %v last_seen_at %d", created.Tags, created.LastSeenAt)
	}
	if !strings.Contains(created.Description, "claude_code") || !strings.Contains(created.Description, "nobody has reviewed") {
		t.Fatalf("description does not say who reported it and that nobody reviewed it: %q", created.Description)
	}
	read, err := s.GetToolByName("claude_code.Read")
	if err != nil {
		t.Fatal(err)
	}
	if !read.Enabled || read.LastSeenAt == 0 || !reflect.DeepEqual(read.Tags, []string{"read-only"}) {
		t.Fatalf("known row: %+v", read)
	}
	// The wire type carries last_seen_at.
	_, listBody := postJSON(t, "GET", srv.URL+"/tools?kind=harness&tag=unreviewed", "")
	if !strings.Contains(string(listBody), `"last_seen_at":`) || !strings.Contains(string(listBody), "claude_code.Frobnicate") {
		t.Fatalf("list: %s", listBody)
	}
}

func TestObservedAgainOnlyMovesLastSeenAt(t *testing.T) {
	srv, s := newServer(t)
	postJSON(t, "POST", srv.URL+"/harness-tools/observed", `{"harness":"claude_code","tool_names":["Frobnicate"]}`)
	// Age the row so a second report is visibly newer.
	if _, err := s.DB().Exec(`UPDATE tools SET last_seen_at = 100, updated_at = 100 WHERE name = 'claude_code.Frobnicate'`); err != nil {
		t.Fatal(err)
	}
	before := dumpRows(t, s.DB(), `SELECT * FROM tools WHERE name = 'claude_code.Frobnicate'`)

	status, body := postJSON(t, "POST", srv.URL+"/harness-tools/observed", `{"harness":"claude_code","tool_names":["Frobnicate","Frobnicate"]}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	response := decodeObserved(t, body)
	if len(response.Created) != 0 || response.Seen != 1 {
		t.Fatalf("response %+v", response)
	}
	after := dumpRows(t, s.DB(), `SELECT * FROM tools WHERE name = 'claude_code.Frobnicate'`)
	var lastSeenAt int64
	s.DB().QueryRow(`SELECT last_seen_at FROM tools WHERE name = 'claude_code.Frobnicate'`).Scan(&lastSeenAt)
	if lastSeenAt <= 100 {
		t.Fatalf("last_seen_at did not move: %d", lastSeenAt)
	}
	if strings.Replace(after[0], fmt.Sprintf("last_seen_at=%d", lastSeenAt), "last_seen_at=100", 1) != before[0] {
		t.Fatalf("a repeat report changed more than last_seen_at:\nbefore %s\nafter  %s", before[0], after[0])
	}
}

func TestObservedConcurrentFirstReportsOfOneNameMakeOneRowAndAllSucceed(t *testing.T) {
	srv, s := newServer(t)
	const reporters = 16
	var wait sync.WaitGroup
	statuses := make([]int, reporters)
	createdCounts := make([]int, reporters)
	start := make(chan struct{})
	for reporter := 0; reporter < reporters; reporter++ {
		wait.Add(1)
		go func(reporter int) {
			defer wait.Done()
			<-start
			response, err := http.Post(srv.URL+"/harness-tools/observed", "application/json",
				strings.NewReader(`{"harness":"claude_code","tool_names":["Read","Frobnicate","Bash"]}`))
			if err != nil {
				statuses[reporter] = -1
				return
			}
			defer response.Body.Close()
			statuses[reporter] = response.StatusCode
			var decoded ObservedHarnessToolsResponse
			json.NewDecoder(response.Body).Decode(&decoded)
			createdCounts[reporter] = len(decoded.Created)
		}(reporter)
	}
	close(start)
	wait.Wait()
	totalCreated := 0
	for reporter, status := range statuses {
		if status != 200 {
			t.Fatalf("reporter %d got status %d", reporter, status)
		}
		totalCreated += createdCounts[reporter]
	}
	if totalCreated != 3 {
		t.Fatalf("created %d rows in total across reporters, want 3", totalCreated)
	}
	var rows int
	s.DB().QueryRow(`SELECT count(*) FROM tools WHERE kind = 'harness'`).Scan(&rows)
	if rows != 3 {
		t.Fatalf("%d harness rows, want 3", rows)
	}
}

func TestObservedRefusesBadReports(t *testing.T) {
	srv, s := newServer(t)
	for _, body := range []string{
		`{"harness":"","tool_names":["Read"]}`,
		`{"tool_names":["Read"]}`,
		`{"harness":"claude_code","tool_names":[]}`,
		`{"harness":"claude_code"}`,
		`{"harness":"claude_code","tool_names":["Read",""]}`,
		`{"harness":"claude_code","tool_names":["a.b"]}`,
		`{"harness":"claude_code","tool_names":["a b"]}`,
		`{"harness":"claude_code","tool_names":["tab\tname"]}`,
		`{"harness":"claude_code","tool_names":["mcp__playwright__click"]}`,
		`{"harness":"claude_code","tool_names":["Read"],"extra":1}`,
		`not json`,
	} {
		status, response := postJSON(t, "POST", srv.URL+"/harness-tools/observed", body)
		if status != 400 {
			t.Errorf("%s: status %d (%s), want 400", body, status, response)
		}
	}
	var rows int
	s.DB().QueryRow(`SELECT count(*) FROM tools`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("a refused report wrote %d rows", rows)
	}
}

func TestObservedRefusesANameAnotherKindHolds(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{Name: "codex.thing", Kind: KindLocal, Local: &LocalSpec{Symbol: "x"}}); err != nil {
		t.Fatal(err)
	}
	status, body := postJSON(t, "POST", srv.URL+"/harness-tools/observed", `{"harness":"codex","tool_names":["thing"]}`)
	if status != 409 {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestPatchToolChangesOnlyTheFieldsSent(t *testing.T) {
	srv, s := newServer(t)
	postJSON(t, "POST", srv.URL+"/harness-tools/observed", `{"harness":"claude_code","tool_names":["Frobnicate"]}`)
	tool, err := s.GetToolByName("claude_code.Frobnicate")
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/tools/%d", srv.URL, tool.ID)

	status, body := postJSON(t, "PATCH", url, `{"tags":["effects","runs-commands"]}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var patched Tool
	json.Unmarshal(body, &patched)
	if !reflect.DeepEqual(patched.Tags, []string{"effects", "runs-commands"}) || patched.Enabled || patched.Description != tool.Description || patched.LastSeenAt != tool.LastSeenAt {
		t.Fatalf("patched %+v", patched)
	}

	status, body = postJSON(t, "PATCH", url, `{"description":"Frobnicates.","enabled":true}`)
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	json.Unmarshal(body, &patched)
	if patched.Description != "Frobnicates." || !patched.Enabled || !reflect.DeepEqual(patched.Tags, []string{"effects", "runs-commands"}) {
		t.Fatalf("patched %+v", patched)
	}

	for _, refused := range []string{`{}`, `{"tag":["x"]}`, `{"tags":null}`, `{"tags":"x"}`, `{"enabled":"yes"}`, `{"tags":[""]}`, `{"name":"other"}`, `not json`} {
		if status, body := postJSON(t, "PATCH", url, refused); status != 400 {
			t.Errorf("%s: status %d (%s), want 400", refused, status, body)
		}
	}
	if status, _ := postJSON(t, "PATCH", srv.URL+"/tools/99999", `{"enabled":false}`); status != 404 {
		t.Errorf("missing tool: status %d, want 404", status)
	}
	after, _ := s.GetTool(tool.ID)
	if after.Description != "Frobnicates." || !after.Enabled || !reflect.DeepEqual(after.Tags, []string{"effects", "runs-commands"}) {
		t.Fatalf("a refused patch changed the row: %+v", after)
	}
}

func TestListNotSeenSince(t *testing.T) {
	srv, s := newServer(t)
	for _, name := range []string{"Old", "Recent", "Never"} {
		if _, err := s.UpsertTool(&Tool{Name: "claude_code." + name, Kind: KindHarness, Harness: "claude_code", HarnessToolName: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpsertTool(&Tool{Name: "end_turn", Kind: KindLocal, Local: &LocalSpec{Symbol: "end_turn"}}); err != nil {
		t.Fatal(err)
	}
	s.DB().Exec(`UPDATE tools SET last_seen_at = 1000 WHERE name = 'claude_code.Old'`)
	s.DB().Exec(`UPDATE tools SET last_seen_at = 5000 WHERE name = 'claude_code.Recent'`)

	status, body := postJSON(t, "GET", srv.URL+"/tools?kind=harness&not_seen_since=2000", "")
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var tools []Tool
	json.Unmarshal(body, &tools)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"claude_code.Never", "claude_code.Old"}) {
		t.Fatalf("not_seen_since=2000 gave %v", names)
	}
	for _, refused := range []string{"/tools?not_seen_since=2000", "/tools?kind=local&not_seen_since=2000", "/tools?kind=harness&not_seen_since=soon", "/tools?kind=harness&not_seen_since=-1"} {
		if status, body := postJSON(t, "GET", srv.URL+refused, ""); status != 400 {
			t.Errorf("%s: status %d (%s), want 400", refused, status, body)
		}
	}
}

func TestUpsertNeverTouchesLastSeenAt(t *testing.T) {
	_, s := newServer(t)
	tool := &Tool{Name: "claude_code.Read", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Read"}
	if _, err := s.UpsertTool(tool); err != nil {
		t.Fatal(err)
	}
	s.DB().Exec(`UPDATE tools SET last_seen_at = 1234 WHERE id = ?`, tool.ID)
	tool.LastSeenAt = 9999
	tool.Description = "changed"
	if _, err := s.UpsertTool(tool); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTool(tool.ID)
	if got.LastSeenAt != 1234 {
		t.Fatalf("upsert wrote last_seen_at: %d", got.LastSeenAt)
	}
}

// A database made after the harness kind but before last_seen_at gets the
// column on open, defined exactly as schema.sql defines it for a new one.
func TestOpenAddsLastSeenAtAsSchemaDefinesIt(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "ts")
	s, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh := dumpRows(t, s.DB(), `SELECT * FROM pragma_table_info('tools') WHERE name = 'last_seen_at'`)
	if len(fresh) != 1 {
		t.Fatalf("a new database has no last_seen_at: %v", fresh)
	}
	tool := &Tool{Name: "claude_code.Read", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Read", Enabled: true}
	if _, err := s.UpsertTool(tool); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE tools DROP COLUMN last_seen_at`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	migrated := dumpRows(t, s.DB(), `SELECT * FROM pragma_table_info('tools') WHERE name = 'last_seen_at'`)
	// cid differs: the added column goes last.
	strip := func(rows []string) string { return rows[0][strings.Index(rows[0], " "):] }
	if len(migrated) != 1 || strip(migrated) != strip(fresh) {
		t.Fatalf("migrated column %v, fresh %v", migrated, fresh)
	}
	got, err := s.GetTool(tool.ID)
	if err != nil || got.LastSeenAt != 0 || !got.Enabled {
		t.Fatalf("row after migration: %+v, %v", got, err)
	}
}
