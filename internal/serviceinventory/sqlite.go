package serviceinventory

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
	_ "modernc.org/sqlite"
)

// openReadOnly opens a database file so that nothing this package runs can
// write to it. mode=ro refuses a write at the file level and query_only
// refuses one at the statement level; both, because the file belongs to a
// running service and this viewer must never be the thing that corrupts or
// locks it.
func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	dsn := "file:" + path + "?" + url.Values{
		"mode":    {"ro"},
		"_pragma": {"query_only(1)", "busy_timeout(2000)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if queryOnly != 1 {
		db.Close()
		return nil, fmt.Errorf("open %s: query_only pragma did not take effect", path)
	}
	return db, nil
}

// quoteIdentifier makes a table or column name safe to splice into SQL. The
// name has already been checked against sqlite_master or table_info, so this
// only guards against a quote inside a legitimate name.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ReadSchema lists every table and view in the file with its DDL, columns
// and row count.
func ReadSchema(ctx context.Context, path string) (*msg.DatabaseSchemaResponse, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range tables {
		columns, err := readColumns(ctx, db, tables[i].Name)
		if err != nil {
			return nil, err
		}
		tables[i].Columns = columns
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(tables[i].Name)).Scan(&count); err != nil {
			tables[i].RowCountError = err.Error()
		} else {
			tables[i].RowCount = count
		}
	}
	return &msg.DatabaseSchemaResponse{Path: path, Tables: tables}, nil
}

func listTables(ctx context.Context, db *sql.DB) ([]msg.DatabaseTable, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, type, sql FROM sqlite_master WHERE type IN ('table','view') ORDER BY rowid")
	if err != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", err)
	}
	defer rows.Close()
	var tables []msg.DatabaseTable
	for rows.Next() {
		var name, kind string
		var ddl sql.NullString
		if err := rows.Scan(&name, &kind, &ddl); err != nil {
			return nil, err
		}
		if kind == "table" && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl.String)), "CREATE VIRTUAL TABLE") {
			kind = "virtual"
		}
		tables = append(tables, msg.DatabaseTable{Name: name, Kind: kind, SQL: ddl.String, Columns: []msg.DatabaseColumn{}})
	}
	if tables == nil {
		tables = []msg.DatabaseTable{}
	}
	return tables, rows.Err()
}

func readColumns(ctx context.Context, db *sql.DB, table string) ([]msg.DatabaseColumn, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, type, \"notnull\", pk FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()
	columns := []msg.DatabaseColumn{}
	for rows.Next() {
		var c msg.DatabaseColumn
		var notNull, pk int
		if err := rows.Scan(&c.Name, &c.Type, &notNull, &pk); err != nil {
			return nil, err
		}
		c.NotNull = notNull != 0
		c.PrimaryKey = pk != 0
		c.Masked = ColumnHoldsCredential(table, c.Name)
		columns = append(columns, c)
	}
	return columns, rows.Err()
}

// RowsQuery is one request for rows of one table.
type RowsQuery struct {
	Table      string
	Limit      int
	OrderBy    string // empty: rowid when the table has one
	Descending bool
	Filters    []msg.DatabaseRowFilter
}

// ParseFilter reads one `column:op:value` filter parameter. The value may
// itself contain colons; only the first two split.
func ParseFilter(text string) (msg.DatabaseRowFilter, error) {
	parts := strings.SplitN(text, ":", 3)
	if len(parts) < 2 {
		return msg.DatabaseRowFilter{}, fmt.Errorf("filter %q: want column:op[:value]", text)
	}
	f := msg.DatabaseRowFilter{Column: parts[0], Op: parts[1]}
	if len(parts) == 3 {
		f.Value = parts[2]
	}
	if !filterOpKnown(f.Op) {
		return msg.DatabaseRowFilter{}, fmt.Errorf("filter %q: op %q is not one of %s", text, f.Op, strings.Join(msg.DatabaseRowFilterOps, ", "))
	}
	return f, nil
}

func filterOpKnown(op string) bool {
	for _, known := range msg.DatabaseRowFilterOps {
		if op == known {
			return true
		}
	}
	return false
}

// A request error is the caller's to fix (unknown table, unknown column);
// anything else is the file's or the host's.
type RequestError struct{ Message string }

func (e *RequestError) Error() string { return e.Message }

// ReadRows answers the newest rows of one table, filtered.
func ReadRows(ctx context.Context, path string, q RowsQuery) (*msg.DatabaseRowsResponse, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}
	var table *msg.DatabaseTable
	for i := range tables {
		if tables[i].Name == q.Table {
			table = &tables[i]
		}
	}
	if table == nil {
		return nil, &RequestError{fmt.Sprintf("no table %q in %s", q.Table, path)}
	}
	columns, err := readColumns(ctx, db, q.Table)
	if err != nil {
		return nil, err
	}
	columnByName := map[string]msg.DatabaseColumn{}
	for _, c := range columns {
		columnByName[c.Name] = c
	}

	var where []string
	var args []any
	for _, f := range q.Filters {
		if _, ok := columnByName[f.Column]; !ok {
			return nil, &RequestError{fmt.Sprintf("no column %q in %s", f.Column, q.Table)}
		}
		col := quoteIdentifier(f.Column)
		switch f.Op {
		case "eq":
			where, args = append(where, col+" = ?"), append(args, f.Value)
		case "ne":
			where, args = append(where, col+" != ?"), append(args, f.Value)
		case "contains":
			where, args = append(where, col+" LIKE ? ESCAPE '\\'"), append(args, "%"+escapeLike(f.Value)+"%")
		case "gt":
			where, args = append(where, col+" > ?"), append(args, f.Value)
		case "gte":
			where, args = append(where, col+" >= ?"), append(args, f.Value)
		case "lt":
			where, args = append(where, col+" < ?"), append(args, f.Value)
		case "lte":
			where, args = append(where, col+" <= ?"), append(args, f.Value)
		case "null":
			where = append(where, col+" IS NULL")
		case "not_null":
			where = append(where, col+" IS NOT NULL")
		default:
			return nil, &RequestError{fmt.Sprintf("filter op %q is not one of %s", f.Op, strings.Join(msg.DatabaseRowFilterOps, ", "))}
		}
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	orderBy := q.OrderBy
	if orderBy == "" {
		if table.Kind == "table" && !strings.Contains(strings.ToUpper(table.SQL), "WITHOUT ROWID") {
			orderBy = "rowid"
		}
	} else if _, ok := columnByName[orderBy]; !ok {
		return nil, &RequestError{fmt.Sprintf("no column %q in %s to order by", orderBy, q.Table)}
	}
	orderSQL := ""
	if orderBy != "" {
		orderSQL = " ORDER BY " + quoteIdentifier(orderBy)
		if q.Descending {
			orderSQL += " DESC"
		}
	}

	var total int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(q.Table)+whereSQL, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count %s: %w", q.Table, err)
	}

	selectList := make([]string, len(columns))
	for i, c := range columns {
		if c.Masked {
			selectList[i] = "NULL"
		} else {
			selectList[i] = quoteIdentifier(c.Name)
		}
	}
	query := "SELECT " + strings.Join(selectList, ", ") + " FROM " + quoteIdentifier(q.Table) + whereSQL + orderSQL + " LIMIT " + strconv.Itoa(q.Limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", q.Table, err)
	}
	defer rows.Close()
	out := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	filters := q.Filters
	if filters == nil {
		filters = []msg.DatabaseRowFilter{}
	}
	return &msg.DatabaseRowsResponse{
		Path: path, Table: q.Table, Columns: columns, Rows: out, TotalRows: total,
		Limit: q.Limit, OrderBy: orderBy, Descending: q.Descending, Filters: filters,
	}, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
