package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/activitystore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/messagestore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enableMessageActivityAndTimeLedgerOnTestDatabase serves message-store,
// activity-store and time-ledger-store from a Postgres database of the test's
// own, after the connected records stores and file-store they follow.
func enableMessageActivityAndTimeLedgerOnTestDatabase(t *testing.T, gated *gatedTestServer) {
	t.Helper()
	enableConnectedRecordsOnTestDatabase(t, gated)
	messageModule, err := messagestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	activityModule, err := activitystore.Module()
	if err != nil {
		t.Fatal(err)
	}
	timeLedgerModule, err := timeledgerstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, messageModule, activityModule, timeLedgerModule)
	if err := gated.server.EnableMessageActivityAndTimeLedgerStores(MessageActivityAndTimeLedgerModules{
		Messages: database.Open(messageModule), Activity: database.Open(activityModule), TimeLedger: database.Open(timeLedgerModule),
	}); err != nil {
		t.Fatal(err)
	}
}

// checkReachedOnlyByOwnerAndAdministrator checks that path answers the owner,
// an administrator and the internal service, is 404 to another principal and
// 401 to nobody.
func checkReachedOnlyByOwnerAndAdministrator(t *testing.T, gated *gatedTestServer, path string) {
	t.Helper()
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the owner's GET %s = %d %s", path, response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", path, nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal's GET %s = %d, want 404", path, response.Code)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET %s = %d %s", path, response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", path); response.Code != http.StatusOK {
		t.Errorf("the internal service's GET %s = %d %s", path, response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
		t.Errorf("GET %s with no credential = %d, want 401", path, response.Code)
	}
}

func TestMessageActivityAndTimeLedgerStoresAnswer503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	for path, code := range map[string]string{"/message-store/messages": "message_store_off", "/activity-store/activities": "activity_store_off", "/time-ledger-store/time-entries": "time_ledger_store_off"} {
		if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), code) {
			t.Errorf("GET %s = %d %s, want 503 %s", path, response.Code, response.Body, code)
		}
	}
}

// A message belongs to whoever filed it: another principal reaches none of
// the conversation, the message or the day, and the internal service acting
// as nobody cannot file one.
func TestAMessageIsReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableMessageActivityAndTimeLedgerOnTestDatabase(t, gated)
	if response := gated.requestAsServiceWithJSON(t, "POST", "/message-store/imports", map[string]any{"importer": "test", "description": "made up"}); response.Code != http.StatusForbidden {
		t.Errorf("the internal service starting an import = %d %s, want 403", response.Code, response.Body)
	}
	started := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/message-store/imports", map[string]any{"importer": "test", "description": "made up"})
	if started.Code != http.StatusCreated && started.Code != http.StatusOK {
		t.Fatalf("start = %d %s", started.Code, started.Body)
	}
	importID, _ := decodeRecord(t, started)["id"].(string)
	batch := map[string]any{
		"conversations": []map[string]any{{"platform": "discord", "source": "discord", "source_ref": "channel-1", "title": "", "is_group": false,
			"participants": []map[string]any{{"handle": "carol", "display_name": "Carol"}, {"handle": "me", "display_name": "Me Self", "is_self": true}}}},
		"messages": []map[string]any{{"conversation_source": "discord", "conversation_source_ref": "channel-1", "sent_at": 1710100800000,
			"sender_handle": "carol", "kind": "text", "body": "hello there", "body_source": "platform", "source": "discord", "source_ref": "m-1", "source_hash": "h1"}},
		"conversation_days": []map[string]any{{"conversation_source": "discord", "conversation_source_ref": "channel-1", "day": "2024-03-10", "time_zone": "Europe/Lisbon"}},
	}
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/message-store/imports/"+importID+"/batches", batch); response.Code != http.StatusOK && response.Code != http.StatusCreated {
		t.Fatalf("batch = %d %s", response.Code, response.Body)
	}
	listed := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/message-store/messages", nil)
	var page struct {
		Messages []struct {
			ID             string `json:"id"`
			ConversationID string `json:"conversation_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || len(page.Messages) != 1 {
		t.Fatalf("the owner's messages = %d %s (%v)", listed.Code, listed.Body, err)
	}
	days := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", "/message-store/conversation-days", nil)
	var dayList []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(days.Body.Bytes(), &dayList); err != nil || len(dayList) != 1 {
		t.Fatalf("the owner's days = %d %s (%v)", days.Code, days.Body, err)
	}
	messageID := page.Messages[0].ID
	checkReachedOnlyByOwnerAndAdministrator(t, gated, "/message-store/messages/"+messageID)
	checkReachedOnlyByOwnerAndAdministrator(t, gated, "/message-store/conversations/"+page.Messages[0].ConversationID)
	checkReachedOnlyByOwnerAndAdministrator(t, gated, "/message-store/conversation-days/"+dayList[0].ID)
	other := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/message-store/messages", nil)
	var otherPage struct {
		Messages []any `json:"messages"`
	}
	if err := json.Unmarshal(other.Body.Bytes(), &otherPage); err != nil || len(otherPage.Messages) != 0 {
		t.Errorf("another principal's messages = %d %s, want none", other.Code, other.Body)
	}

	// A chip of the message needs the grant and the owner, asked in process.
	resolved := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{messageID}})
	if !strings.Contains(resolved.Body.String(), "hello there") {
		t.Errorf("the administrator resolving the message = %d %s", resolved.Code, resolved.Body)
	}
	if hidden := gated.requestAsPrincipal(t, secondTestPrincipalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{messageID}}); strings.Contains(hidden.Body.String(), "hello there") {
		t.Errorf("another principal resolving the message = %d %s", hidden.Code, hidden.Body)
	}
}

// An attachment's bytes go to file-store, owned by activity-store, and come
// back to the activity's owner only.
func TestAnActivityAttachmentIsKeptInFileStore(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableMessageActivityAndTimeLedgerOnTestDatabase(t, gated)
	files := ActivityAttachmentFilesIn(gated.server.FileStoreModuleHandler())
	fileID, err := files.Put(t.Context(), strings.NewReader("made-up recording"), activitystore.AttachmentFile{ActivityID: "activity_000001", FileName: "clip.mp3", ContentType: "audio/mpeg"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fileID, "file_") {
		t.Fatalf("file-store answered id %q", fileID)
	}
	content, err := files.Open(t.Context(), fileID)
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(content)
	content.Close()
	if err != nil || string(read) != "made-up recording" {
		t.Fatalf("read back %q, %v", read, err)
	}
	record := gated.requestAsService(t, "GET", "/file-store/files/"+fileID)
	if !strings.Contains(record.Body.String(), `"owner_service":"activity-store"`) || !strings.Contains(record.Body.String(), `"owner_ref":"activity_000001"`) {
		t.Errorf("the file-store record = %d %s", record.Code, record.Body)
	}
	if err := files.Purge(t.Context(), fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Open(t.Context(), fileID); err == nil {
		t.Error("a purged file still opens")
	}
}

// A time entry belongs to whoever made it.
func TestATimeEntryIsReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableMessageActivityAndTimeLedgerOnTestDatabase(t, gated)
	entry := map[string]any{"title": "Lunch", "starts_at": 1710100800, "ends_at": 1710104400, "by": "test", "by_kind": "operator", "reason": "made up for a test", "time_zone": "Europe/Lisbon"}
	if response := gated.requestAsServiceWithJSON(t, "POST", "/time-ledger-store/time-entries", entry); response.Code != http.StatusForbidden {
		t.Errorf("the internal service making an entry = %d %s, want 403", response.Code, response.Body)
	}
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/time-ledger-store/time-entries", entry)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	record := decodeRecord(t, created)
	id, _ := record["id"].(string)
	if record["owner_principal_id"] != firstTestPrincipalID {
		t.Errorf("the entry's owner is %v", record["owner_principal_id"])
	}
	checkReachedOnlyByOwnerAndAdministrator(t, gated, "/time-ledger-store/time-entries/"+id)
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/time-ledger-store/time-entries", nil); response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Errorf("another principal's list = %d %s, want an empty list", response.Code, response.Body)
	}
}
