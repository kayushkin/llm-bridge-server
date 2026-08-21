package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// Both create paths in this file guard a column that also carries a uniqueness
// constraint in SQLite: machines.name is `TEXT NOT NULL UNIQUE` (harness-store
// store.go), and sessions.bridge_id is the table's `TEXT PRIMARY KEY`
// (internal/store/store.go), which SQLite enforces as a unique index and
// reports as `UNIQUE constraint failed:`. The index answers last, so a test
// that asserts only "an error came back" cannot tell the Go guard from the
// constraint and stays green with the guard deleted.
//
// What separates the two layers here is the STATUS, not only the wording. The
// Go guard answers 409 — a conflict the caller can correct by picking another
// name or id. With the guard gone the insert reaches SQLite, the constraint
// rejects it, and the handler surfaces that through the generic
// `http.Error(w, err.Error(), http.StatusInternalServerError)` arm as a 500
// carrying the raw driver text. So each test below asserts the status AND the
// guard's own wording, and neither assertion survives the guard's deletion.
//
// There is no side effect that separates the layers on either path, which is
// worth stating rather than leaving implied: on both, the guard sits
// immediately above the insert it protects and there is no spend, network call
// or write between them, so a guard that slid below the insert would be
// rejected by the insert itself rather than doing damage first. Where a guard
// shadows an index AND the two differ in a side effect, assert the side effect
// instead — it catches reordering, which a status-and-wording pair does not.

func TestCreateMachineRefusesANameAlreadyInUse(t *testing.T) {
	// testServerWithInstance seeds exactly one machine, named "test-machine".
	srv, _, _ := testServerWithInstance(t, msg.HarnessClaudeCode)

	resp := doJSON(t, srv, "POST", "/machines", msg.CreateMachineRequest{
		Name:      "test-machine",
		Transport: msg.TransportLocal,
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 409 is the Go guard's answer. The UNIQUE index reaches the caller as a
	// 500, so this comparison is what makes the test able to fail when the
	// guard is deleted.
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d — a 500 here means the UNIQUE index answered, not the Go guard: %s",
			resp.StatusCode, http.StatusConflict, body)
	}
	// The wording is the guard's own. `UNIQUE constraint failed: machines.name`
	// is the index's, and names no record.
	if want := "machine name already in use"; !strings.Contains(string(body), want) {
		t.Errorf("body = %q, want it to contain %q (the Go guard's wording)", body, want)
	}
	// The duplicate must not have been stored under a second id. This is an
	// invariant, not a discriminator: it cannot redden under the guard-only
	// mutation, because the UNIQUE index rejects the insert whether or not the
	// guard ran. Only deleting the index too would move it, and deleting the
	// index proves nothing about the test. Stated rather than dressed up.
	list := doJSON(t, srv, "GET", "/machines", nil)
	defer list.Body.Close()
	machines := decodeJSON[[]*msg.Machine](t, list)
	named := 0
	for _, m := range machines {
		if m.Name == "test-machine" {
			named++
		}
	}
	if named != 1 {
		t.Errorf("machines named %q = %d, want 1", "test-machine", named)
	}
}

func TestCreateSessionRefusesACallerMintedIDThatAlreadyExists(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)

	create := func() *http.Response {
		return doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
			Type:       msg.SessionTypeInteractive,
			Purpose:    msg.PurposeChat,
			Origin:     "test",
			Harness:    "claude_code",
			InstanceID: instID,
			SessionID:  "br_caller_minted",
		})
	}

	first := create()
	defer first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(first.Body)
		t.Fatalf("first create: status = %d, want 201: %s", first.StatusCode, body)
	}

	second := create()
	defer second.Body.Close()
	body, _ := io.ReadAll(second.Body)

	if second.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d — a 500 here means the bridge_id PRIMARY KEY answered, not the Go guard: %s",
			second.StatusCode, http.StatusConflict, body)
	}
	if want := "session_id already exists"; !strings.Contains(string(body), want) {
		t.Errorf("body = %q, want it to contain %q (the Go guard's wording)", body, want)
	}

	// The first session must be untouched — same row, not overwritten by the
	// second request's fields. Same standing as the machine count above: the
	// PRIMARY KEY blocks the second insert on its own, so this cannot redden
	// under the guard-only mutation either.
	sess, err := st.GetSession("br_caller_minted")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Origin != "test" {
		t.Errorf("origin = %q, want %q", sess.Origin, "test")
	}
}
