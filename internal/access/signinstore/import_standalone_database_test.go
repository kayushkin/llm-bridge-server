package signinstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store kept
// one, with a few made-up rows: a redeemed link and its session, an unspent
// link, a revoked session, a spent Discord attempt, and gaps in the numbering.
// It returns the path and the cookie secret of the live session.
func newStandaloneDatabase(t *testing.T) (string, string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sign-in-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	liveSecret := "a-live-session-secret"
	statements := []string{
		`INSERT INTO sign_in_links VALUES (1, '` + hashSecret("spent-link") + `', 'principal_000002', 'operator', 'principal_000001', 1800000000, 1800604800, 1800000100, 'session_000002')`,
		`INSERT INTO sign_in_links VALUES (3, '` + hashSecret("open-link") + `', 'principal_000002', 'discord', '', 1800000200, 4000000000, 0, '')`,
		`INSERT INTO sessions VALUES (2, '` + hashSecret(liveSecret) + `', 'principal_000002', 'sign_in_link', 'sign_in_link_000001', 1800000100, 4000000000, 1800000500, 0, '', '', 'Firefox', '203.0.113.7')`,
		`INSERT INTO sessions VALUES (5, '` + hashSecret("revoked") + `', 'principal_000001', 'discord', '', 1800000300, 4000000000, 1800000300, 1800000400, 'principal_000001', 'lost phone', 'Safari', '198.51.100.4')`,
		`INSERT INTO discord_attempts VALUES (4, 'state-hash', 'browser-hash', 'link', 'principal_000001', 'session_000005', 'https://dash.kayushkin.com/', 1800000300, 1800000900, 1800000350)`,
		// A row added and removed afterwards: AUTOINCREMENT remembers 7.
		`INSERT INTO sessions VALUES (7, 'gone', 'principal_000001', 'discord', '', 1, 2, 1, 0, '', '', '', '')`,
		`DELETE FROM sessions WHERE seq = 7`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path, liveSecret
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	path, liveSecret := newStandaloneDatabase(t)
	counts, err := ImportStandaloneDatabase(ctx, store.db, path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{SignInLinks: 2, Sessions: 2, DiscordAttempts: 1, LastSignInLinkSeq: 3, LastSessionSeq: 5, LastDiscordAttemptSeq: 4}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	live, err := store.SessionBySecret(ctx, liveSecret)
	if err != nil || live.ID != "session_000002" || live.SignInLinkID != "sign_in_link_000001" || live.UserAgent != "Firefox" || live.LastSeenAt != 1800000500 {
		t.Fatalf("the live session by its cookie = %+v %v", live, err)
	}
	revoked, err := store.GetSession(ctx, "session_000005")
	if err != nil || revoked.RevokedAt != 1800000400 || revoked.RevokeReason != "lost phone" || revoked.Method != MethodDiscord {
		t.Fatalf("the revoked session = %+v %v", revoked, err)
	}
	links, err := store.ListSignInLinks(ctx, "", 10)
	if err != nil || len(links) != 2 || links[0].ID != "sign_in_link_000003" || links[1].RedeemedAt != 1800000100 || links[1].SessionID != "session_000002" || links[1].MadeBy != "principal_000001" {
		t.Fatalf("links = %+v %v", links, err)
	}
	// The unspent link still signs in, once.
	session, _, err := store.RedeemSignInLink(ctx, "open-link", SessionRequestFacts{}, func(string) error { return nil })
	if err != nil || session.ID != "session_000008" {
		t.Fatalf("redeeming the imported link = %+v %v, want session_000008: numbering goes on after the 7 SQLite handed out", session, err)
	}
	if _, _, err := store.RedeemSignInLink(ctx, "spent-link", SessionRequestFacts{}, func(string) error { return nil }); !errors.Is(err, ErrLinkNotUsable) {
		t.Fatalf("redeeming the imported spent link = %v, want ErrLinkNotUsable", err)
	}
	link, _, err := store.MakeSignInLink(ctx, "principal_000002", SentByOperator, "principal_000001")
	if err != nil || link.ID != "sign_in_link_000004" {
		t.Fatalf("a new link = %+v %v, want sign_in_link_000004", link, err)
	}
	if _, err := store.spendDiscordAttempt(ctx, "state-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("spending the imported spent attempt = %v, want ErrNotFound", err)
	}

	if _, err := ImportStandaloneDatabase(ctx, store.db, path); err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("a second import = %v, want a refusal", err)
	}
}

func TestImportRefusesATableWithAColumnItDoesNotKnow(t *testing.T) {
	store := newTestStore(t)
	path, _ := newStandaloneDatabase(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN device TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(context.Background(), store.db, path); err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("import = %v, want a refusal naming the column", err)
	}
	var held int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sign_in_links`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("links after a refused import = %d (%v), want 0", held, err)
	}
}
