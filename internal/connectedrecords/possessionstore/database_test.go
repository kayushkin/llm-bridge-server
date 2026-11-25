package possessionstore

import "testing"

func TestPlaceholdersAreNumberedOutsideQuotesAndComments(t *testing.T) {
	for query, want := range map[string]string{
		`SELECT 1`: `SELECT 1`,
		`SELECT id FROM t WHERE a = ? AND b IN (?, ?)`:                 `SELECT id FROM t WHERE a = $1 AND b IN ($2, $3)`,
		`SELECT '?' || "odd?name" FROM t WHERE a = ?`:                  `SELECT '?' || "odd?name" FROM t WHERE a = $1`,
		`SELECT 'it''s ?' FROM t WHERE a = ? -- why?` + "\n AND b = ?": `SELECT 'it''s ?' FROM t WHERE a = $1 -- why?` + "\n AND b = $2",
		`UPDATE t SET a = ?1 WHERE b = ?1`:                             `UPDATE t SET a = $1 WHERE b = $1`,
		`SELECT $1::text`:                                              `SELECT $1::text`,
		`SELECT ?::bigint`:                                             `SELECT $1::bigint`,
	} {
		if got := numberPlaceholders(query); got != want {
			t.Errorf("numberPlaceholders(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestABoolArgumentIsPassedAsSQLiteKeptIt(t *testing.T) {
	arguments := []any{"x", true, false, 3}
	converted := postgresArguments(arguments)
	if converted[1] != 1 || converted[2] != 0 || converted[0] != "x" || converted[3] != 3 {
		t.Fatalf("converted = %v", converted)
	}
	if arguments[1] != true {
		t.Fatal("the caller's arguments must not change")
	}
}
