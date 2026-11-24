package usagestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

// A database/sql row iteration can fail AFTER the first row has been handed
// over: a dropped connection, a corrupt page, a canceled context. `rows.Next`
// reports that by returning false, exactly as it does at a clean end of
// results, and the only thing that tells the two apart is `rows.Err()`.
//
// Commit 6382825 swept usage.go for this and added the check to Query and
// Summary. spend.go was missed, so the three loops that back the Usage page
// answered a truncated read as a complete one, with a nil error. These tests
// reach that path with a driver that fails mid-iteration on demand.

// errRowIterationFailed is what the fake driver raises partway through a result
// set. It is not io.EOF, so database/sql stores it rather than treating it as a
// clean end of rows.
var errRowIterationFailed = errors.New("connection lost mid-result-set")

// failingRows serves rowsBeforeFailure rows and then fails, which is the shape
// that makes an unchecked loop return a short list and a nil error.
type failingRows struct {
	columns           []string
	row               []driver.Value
	rowsBeforeFailure int
	served            int
}

func (r *failingRows) Columns() []string { return r.columns }
func (r *failingRows) Close() error      { return nil }

func (r *failingRows) Next(dest []driver.Value) error {
	if r.served >= r.rowsBeforeFailure {
		return errRowIterationFailed
	}
	r.served++
	copy(dest, r.row)
	return nil
}

// exhaustedRows ends cleanly after its rows, so a suite can tell "the check
// fired because the read failed" from "the check fires on every read".
type exhaustedRows struct {
	columns []string
	row     []driver.Value
	served  int
	total   int
}

func (r *exhaustedRows) Columns() []string { return r.columns }
func (r *exhaustedRows) Close() error      { return nil }

func (r *exhaustedRows) Next(dest []driver.Value) error {
	if r.served >= r.total {
		return io.EOF
	}
	r.served++
	copy(dest, r.row)
	return nil
}

// scriptedConn hands every query the same scripted result set, so a test names
// the shape it wants and does not care which SQL the store sends.
type scriptedConn struct{ newRows func() driver.Rows }
type scriptedStmt struct{ newRows func() driver.Rows }

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) {
	return &scriptedStmt{newRows: c.newRows}, nil
}
func (c *scriptedConn) Close() error              { return nil }
func (c *scriptedConn) Begin() (driver.Tx, error) { return nil, errors.New("no transactions here") }

func (s *scriptedStmt) Close() error  { return nil }
func (s *scriptedStmt) NumInput() int { return -1 }
func (s *scriptedStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("no writes here")
}
func (s *scriptedStmt) Query([]driver.Value) (driver.Rows, error) { return s.newRows(), nil }

// storeReadingScriptedRows builds a Store whose every query answers with the
// given result set. It writes the db field directly rather than going through
// Open, which is the whole reason this test lives in package usagestore.
func storeReadingScriptedRows(t *testing.T, newRows func() driver.Rows) *Store {
	t.Helper()
	db := sql.OpenDB(scriptedDriverConnector{newRows: newRows})
	t.Cleanup(func() { db.Close() })
	return &Store{db: db}
}

type scriptedDriverConnector struct{ newRows func() driver.Rows }

func (c scriptedDriverConnector) Connect(context.Context) (driver.Conn, error) {
	return &scriptedConn{newRows: c.newRows}, nil
}
func (c scriptedDriverConnector) Driver() driver.Driver { return scriptedDriver{} }

type scriptedDriver struct{}

func (scriptedDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("open by connector only")
}

func assertIterationFailureIsReported(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s answered a truncated read with a nil error: the caller cannot tell a short list from a complete one", what)
	}
	if !strings.Contains(err.Error(), errRowIterationFailed.Error()) {
		t.Fatalf("%s reported an error that is not the iteration failure: %v", what, err)
	}
}

func TestPerKeyTotalsReportsAnIterationFailure(t *testing.T) {
	store := storeReadingScriptedRows(t, func() driver.Rows {
		return &failingRows{
			columns:           []string{"api_key_id", "SUM(total_usd)"},
			row:               []driver.Value{"k1", 250.0},
			rowsBeforeFailure: 1,
		}
	})
	_, err := store.PerKeyTotals("anthropic", 0)
	assertIterationFailureIsReported(t, "PerKeyTotals", err)
}

func TestListKeyMetaReportsAnIterationFailure(t *testing.T) {
	store := storeReadingScriptedRows(t, func() driver.Rows {
		return &failingRows{
			columns:           []string{"provider", "api_key_id", "api_key_name", "api_key_hint", "api_key_status", "fetched_at"},
			row:               []driver.Value{"anthropic", "k1", "primary", "sk-...k1", "active", int64(1)},
			rowsBeforeFailure: 1,
		}
	})
	_, err := store.ListKeyMeta("anthropic")
	assertIterationFailureIsReported(t, "ListKeyMeta", err)
}

func TestListTopupsReportsAnIterationFailure(t *testing.T) {
	store := storeReadingScriptedRows(t, func() driver.Rows {
		return &failingRows{
			columns:           []string{"id", "provider", "amount_usd", "occurred_at", "note", "created_at"},
			row:               []driver.Value{int64(1), "anthropic", 300.0, int64(5), "", int64(5)},
			rowsBeforeFailure: 1,
		}
	})
	_, err := store.ListTopups("anthropic")
	assertIterationFailureIsReported(t, "ListTopups", err)
}

// The control: the same driver, ending cleanly. If these fail, the three tests
// above are passing because the check fires on every read, not because it
// detects a failure.
func TestACleanlyEndedReadIsNotReportedAsAFailure(t *testing.T) {
	store := storeReadingScriptedRows(t, func() driver.Rows {
		return &exhaustedRows{
			columns: []string{"api_key_id", "SUM(total_usd)"},
			row:     []driver.Value{"k1", 250.0},
			total:   2,
		}
	})
	totals, err := store.PerKeyTotals("anthropic", 0)
	if err != nil {
		t.Fatalf("a clean read was reported as a failure: %v", err)
	}
	if got := totals["k1"]; got != 250.0 {
		t.Fatalf("clean read lost its row: got %v want 250", got)
	}
}
