package principals

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store kept
// one, FTS5 index and triggers included, with a few made-up rows: a human
// administrator with a working week, a disabled human, a group with members,
// a contact, absences and two bindings, one unlinked, with gaps in every
// numbering.
func newStandaloneDatabase(t *testing.T, extraStatements ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "principal-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// One connection, so the foreign-key switch below holds for every
	// statement: a broken file is made by turning the check off first.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO principals VALUES ('principal_000001', 1, 'human', 'Priya Raman', 'priya.raman@northwind-eng.example', 0, 1800000000, 1800000100, '{"tzid":"Europe/Amsterdam","days":["MO","TU"],"start":"09:00","end":"17:00"}', 1)`,
		`INSERT INTO principals VALUES ('principal_000002', 2, 'group', 'Data Team', '', 0, 1800000010, 1800000010, '', 0)`,
		`INSERT INTO principals VALUES ('principal_000004', 4, 'human', 'Marcus Feld', 'marcus@example.test', 1800000500, 1800000020, 1800000500, '', 0)`,
		`INSERT INTO principals VALUES ('principal_000005', 5, 'contact', 'requester@example.test', 'requester@example.test', 0, 1800000030, 1800000030, '', 0)`,
		`INSERT INTO group_members VALUES ('principal_000002', 'principal_000001', 1800000040)`,
		`INSERT INTO group_members VALUES ('principal_000002', 'principal_000004', 1800000041)`,
		`INSERT INTO time_off VALUES ('timeoff_000003', 3, 'principal_000001', 1800100000, 1800200000, 'holiday', 1800000050)`,
		`INSERT INTO identity_bindings VALUES ('identity_binding_000002', 2, 'principal_000001', 'discord', '', '123456789012345678', 1800000060, 'principal_000001', 'first', 1800000070, 'principal_000001', 'wrong account')`,
		`INSERT INTO identity_bindings VALUES ('identity_binding_000003', 3, 'principal_000001', 'discord', '', '223456789012345678', 1800000080, 'principal_000001', '', 0, '', '')`,
	}
	if len(extraStatements) > 0 {
		statements = append(statements, "PRAGMA foreign_keys = OFF")
	}
	for _, statement := range append(statements, extraStatements...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	s := newTestStore(t)
	counts, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Principals: 4, DisabledPrincipals: 1, Memberships: 2, TimeOff: 1, IdentityBindings: 2,
		LastPrincipalSeq: 5, LastTimeOffSeq: 3, LastIdentityBinding: 3}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	priya, err := s.Get(testContext, "principal_000001", false)
	if err != nil {
		t.Fatal(err)
	}
	if !priya.IsAdministrator || priya.Availability == nil || priya.Availability.TZID != "Europe/Amsterdam" || priya.CreatedAt != 1800000000 || priya.UpdatedAt != 1800000100 {
		t.Fatalf("principal_000001 = %+v", priya)
	}
	if len(priya.Groups) != 1 || priya.Groups[0].ID != "principal_000002" {
		t.Fatalf("groups = %+v", priya.Groups)
	}
	team, err := s.Get(testContext, "principal_000002", true)
	if err != nil || len(team.Members) != 2 {
		t.Fatalf("members with the disabled one = %+v %v", team, err)
	}
	marcus, err := s.Get(testContext, "principal_000004", false)
	if err != nil || marcus.DisabledAt != 1800000500 {
		t.Fatalf("disabled principal = %+v %v", marcus, err)
	}
	timeOff, err := s.ListTimeOff(testContext, "principal_000001")
	if err != nil || len(timeOff) != 1 || timeOff[0].ID != "timeoff_000003" || timeOff[0].Note != "holiday" {
		t.Fatalf("time off = %+v %v", timeOff, err)
	}
	unlinked, err := s.GetIdentityBinding(testContext, "identity_binding_000002")
	if err != nil || unlinked.UnlinkedBy != "principal_000001" || unlinked.UnlinkReason != "wrong account" || unlinked.LinkedBy != "principal_000001" {
		t.Fatalf("unlinked binding = %+v %v", unlinked, err)
	}
	resolved, err := s.ResolveIdentity(testContext, "discord", "", "223456789012345678")
	if err != nil || resolved.Binding.ID != "identity_binding_000003" || resolved.Binding.UnlinkedBy != "" {
		t.Fatalf("active binding = %+v %v", resolved, err)
	}
	if found, err := s.List(testContext, Filter{Query: "northwind"}); err != nil || len(found) != 1 {
		t.Fatalf("search over imported rows = %v %v", found, err)
	}

	// Numbering goes on after the highest number copied, so no id handed out
	// before is handed out again.
	next := mustCreate(t, s, KindHuman, "Helena Vos", "")
	if next.ID != "principal_000006" {
		t.Fatalf("next principal = %s, want principal_000006", next.ID)
	}
	off, err := s.AddTimeOff(testContext, next.ID, 1800300000, 1800400000, "")
	if err != nil || off.ID != "timeoff_000004" {
		t.Fatalf("next time off = %+v %v", off, err)
	}
	binding, err := s.LinkIdentity(testContext, next.ID, "discord", "", "323456789012345678", "principal_000001", "")
	if err != nil || binding.ID != "identity_binding_000004" {
		t.Fatalf("next binding = %+v %v", binding, err)
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, KindHuman, "Already Here", "")
	_, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t))
	if err == nil || !strings.Contains(err.Error(), "empty module") {
		t.Fatalf("err = %v, want a refusal of a module that holds rows", err)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	for name, statement := range map[string]string{
		"an id that is not its number's":    `INSERT INTO principals VALUES ('principal_000009', 8, 'human', 'Off By One', '', 0, 1, 1, '', 0)`,
		"an unknown column":                 `ALTER TABLE principals ADD COLUMN nickname TEXT NOT NULL DEFAULT ''`,
		"an unknown table":                  `CREATE TABLE principal_resources (principal_id TEXT)`,
		"a membership of an odd id":         `INSERT INTO group_members VALUES ('principal_000002', 'someone', 1)`,
		"an unknown kind":                   `INSERT INTO principals VALUES ('principal_000010', 10, 'robot', 'Marvin', '', 0, 1, 1, '', 0)`,
		"a binding unlinked by nobody":      `INSERT INTO identity_bindings VALUES ('identity_binding_000007', 7, 'principal_000001', 'discord', '', '423456789012345678', 1, 'principal_000001', '', 5, '', 'gone')`,
		"a time off on a missing principal": `INSERT INTO time_off VALUES ('timeoff_000008', 8, 'principal_000099', 1, 2, '', 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			if _, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t, statement)); err == nil {
				t.Fatal("import succeeded, want a refusal")
			}
			if counts, err := s.Counts(testContext); err != nil || counts.Principals != 0 {
				t.Fatalf("after a refused import the module holds %+v (%v); want nothing", counts, err)
			}
		})
	}
}
