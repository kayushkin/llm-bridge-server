package machines

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneTable is one table of the SQLite harness-store file: its columns
// as SQLite names them, the module's column each is copied into, and how each
// value is read.
type standaloneTable struct {
	name    string
	orderBy string
	columns []standaloneColumn
}

type standaloneColumn struct {
	sqliteName   string
	postgresName string
	read         func(column string, value any) (any, error)
}

func column(name string, read func(string, any) (any, error)) standaloneColumn {
	return standaloneColumn{sqliteName: name, postgresName: name, read: read}
}

// standaloneTables are the tables of the SQLite file, once the server's
// start-up migrations had run, in the order they are copied. A file whose
// instances table still has the old host column is refused: open it with a
// server from before the move once, so it migrates it, and import the result.
var standaloneTables = []standaloneTable{
	{name: "harness_types", orderBy: "name", columns: []standaloneColumn{
		column("name", text), column("label", text), column("emoji", text), column("image", text), column("updated_at", timestamp),
	}},
	{name: "machines", orderBy: "id", columns: []standaloneColumn{
		column("id", text), column("name", text), column("emoji", text), column("hostname", text), column("os", text),
		column("arch", text), column("transport", text), column("ssh_user", text), column("ssh_key_path", text),
		column("ssh_port", integer), column("default_working_dir", text),
		{sqliteName: "user", postgresName: "operating_system_user", read: text},
		column("notes", text), column("computer_use_allowed", boolean), column("runner_token_hash", text),
		column("last_seen_at", nullableTimestamp), column("created_at", timestamp), column("updated_at", timestamp),
	}},
	{name: "runner_enrollments", orderBy: "id", columns: []standaloneColumn{
		column("id", text), column("passphrase_hash", text), column("expires_at", timestamp),
		column("used_at", nullableTimestamp), column("consumed_machine_id", nullableText), column("created_at", timestamp),
	}},
	{name: "instances", orderBy: "id", columns: []standaloneColumn{
		column("id", text), column("harness_type", text), column("name", text), column("machine_id", text),
		column("working_dir", text), column("max_concurrent_sessions", integer), column("enabled", boolean),
		column("created_at", timestamp), column("updated_at", timestamp),
	}},
	{name: "instance_credentials", orderBy: "instance_id, credential_id", columns: []standaloneColumn{
		column("instance_id", text), column("credential_id", text), column("priority", integer),
		column("max_concurrent", integer), column("enabled", boolean),
	}},
}

// columnsAStoreMayLack are columns an older file has and a file this server
// created has not: instance_credentials.max_concurrent was in the first
// schema and dropped from the one fresh files get. A file without it takes
// the module's default.
var columnsAStoreMayLack = map[string]map[string]bool{"instance_credentials": {"max_concurrent": true}}

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	HarnessTypes        int64 `json:"harness_types"`
	Machines            int64 `json:"machines"`
	RunnerEnrollments   int64 `json:"runner_enrollments"`
	Instances           int64 `json:"instances"`
	InstanceCredentials int64 `json:"instance_credentials"`
}

// ImportStandaloneDatabase copies every row of the SQLite harness-store file
// into the module behind db, as it is: machine, instance and enrollment ids,
// names, runner token hashes, switches and times. Postgres keeps times to the
// microsecond, so a time's nanoseconds are cut to microseconds. Rows that
// name a machine or instance the file no longer holds are copied as they are,
// since nothing ever enforced those references. It runs in one transaction,
// so it copies everything or nothing, and it refuses a module that already
// holds any row and a file it cannot copy whole.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	if err := refuseUnknownStandaloneTables(ctx, source, sqlitePath); err != nil {
		return counts, err
	}
	tablesToCopy := make([]standaloneTable, 0, len(standaloneTables))
	for _, table := range standaloneTables {
		present, err := tableColumns(ctx, source, table.name)
		if err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		var kept []standaloneColumn
		var names []string
		for _, column := range table.columns {
			if !present[column.sqliteName] && columnsAStoreMayLack[table.name][column.sqliteName] {
				continue
			}
			kept = append(kept, column)
			names = append(names, column.sqliteName)
		}
		if err := standalonesqlite.CheckColumns(ctx, source, "harness-store", table.name, names); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		tablesToCopy = append(tablesToCopy, standaloneTable{name: table.name, orderBy: table.orderBy, columns: kept})
	}

	copied := map[string]int64{}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "machines",
			"harness_types", "machines", "runner_enrollments", "instances", "instance_credentials"); err != nil {
			return err
		}
		for _, table := range tablesToCopy {
			sqliteNames := make([]string, len(table.columns))
			insert := "INSERT INTO " + table.name + " ("
			values := ""
			for index, column := range table.columns {
				sqliteNames[index] = column.sqliteName
				if index > 0 {
					insert += ", "
					values += ", "
				}
				insert += column.postgresName
				values += fmt.Sprintf("$%d", index+1)
			}
			columns := table.columns
			count, err := standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
				Name: table.name, Columns: sqliteNames, OrderBy: table.orderBy,
				Insert: insert + ") VALUES (" + values + ")",
				Convert: func(row []any) ([]any, error) {
					converted := make([]any, len(row))
					for index, column := range columns {
						value, err := column.read(column.sqliteName, row[index])
						if err != nil {
							return nil, err
						}
						converted[index] = value
					}
					return converted, nil
				},
			})
			if err != nil {
				return err
			}
			copied[table.name] = count
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return ImportCounts{
		HarnessTypes: copied["harness_types"], Machines: copied["machines"], RunnerEnrollments: copied["runner_enrollments"],
		Instances: copied["instances"], InstanceCredentials: copied["instance_credentials"],
	}, nil
}

func tableColumns(ctx context.Context, source *sql.DB, table string) (map[string]bool, error) {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		present[name] = true
	}
	return present, rows.Err()
}

// refuseUnknownStandaloneTables refuses a file holding a table the import
// does not copy: its rows would be lost without a word.
func refuseUnknownStandaloneTables(ctx context.Context, source *sql.DB, sqlitePath string) error {
	known := map[string]bool{}
	for _, table := range standaloneTables {
		known[table.name] = true
	}
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
		}
		if !known[name] {
			return fmt.Errorf("%s has a table %s, which the import does not know; it copies only a file it can copy whole", sqlitePath, name)
		}
	}
	return rows.Err()
}

func text(column string, value any) (any, error) { return standalonesqlite.Text(column, value) }

func nullableText(column string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	return standalonesqlite.Text(column, value)
}

func integer(column string, value any) (any, error) { return standalonesqlite.Integer(column, value) }

func boolean(column string, value any) (any, error) { return standalonesqlite.Boolean(column, value) }

// timestamp reads a DATETIME column, which the SQLite driver hands back as a
// time when its text parses as one. Text that does not parse is refused
// rather than guessed at.
func timestamp(column string, value any) (any, error) {
	at, ok := value.(time.Time)
	if !ok {
		return nil, fmt.Errorf("%s is %T %v, not a time", column, value, value)
	}
	return at.UTC(), nil
}

func nullableTimestamp(column string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	return timestamp(column, value)
}
