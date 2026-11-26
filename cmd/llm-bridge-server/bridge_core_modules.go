package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// openBridgeCoreStore provisions, migrates and opens the bridge's own records
// (the bridge_core module) on the Postgres at url: its writer pool and a
// read-only pool beside it.
//
// It refuses to start on an empty module while a bridge database is still at
// bridgeDatabasePath: that server's sessions, sign-ins, peers and settings
// would all be left behind, and the first start makes a new server id, after
// which the import refuses the module. Run import-bridge-database first.
func openBridgeCoreStore(ctx context.Context, url, bridgeDatabasePath string) (*store.Store, error) {
	if url == "" {
		return nil, fmt.Errorf("%s is not set; the bridge keeps its own records in Postgres", config.PostgresURLEnvironmentVariable)
	}
	module, err := store.Module()
	if err != nil {
		return nil, err
	}
	writer, err := openPostgresModule(ctx, url, module, postgresstorage.ScanTimestampsInUTC)
	if err != nil {
		return nil, err
	}
	holds, err := store.ModuleHoldsRecords(ctx, writer)
	if err != nil {
		writer.Close()
		return nil, err
	}
	if !holds {
		if info, err := os.Stat(bridgeDatabasePath); err == nil && info.Size() > 0 {
			writer.Close()
			return nil, fmt.Errorf("the bridge core module is empty and a bridge database is at %s; copy it in first with `llm-bridge-server import-bridge-database -sqlite <copy of it>`", bridgeDatabasePath)
		}
	}
	reader, err := store.OpenReader(ctx, url, module)
	if err != nil {
		writer.Close()
		return nil, err
	}
	opened, err := store.New(writer, reader)
	if err != nil {
		writer.Close()
		reader.Close()
		return nil, err
	}
	return opened, nil
}

// importBridgeDatabaseCmd copies the bridge's SQLite database (bridge.db, or a
// copy of it without its events table) into the bridge core module, keeping
// every id and number, and records the highest id its events table handed
// out. It refuses a module that already holds any row.
func importBridgeDatabaseCmd(args []string) {
	flags := flag.NewFlagSet("import-bridge-database", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the bridge database to copy: a copy of bridge.db made while the server is stopped, or one made with sqlite3 .backup; its events table may be left out")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-bridge-database -sqlite <file>")
		os.Exit(2)
	}
	ctx := context.Background()
	module, err := store.Module()
	if err != nil {
		importFailed(err)
	}
	db, err := openPostgresModule(ctx, importPostgresURL(), module, postgresstorage.ScanTimestampsInUTC)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := store.ImportBridgeDatabase(ctx, db, *sqlitePath, "import-bridge-database -sqlite "+*sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	tables := make([]string, 0, len(counts.RowsByTable))
	for table := range counts.RowsByTable {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		fmt.Printf("%-28s %d\n", table+":", counts.RowsByTable[table])
	}
	for _, value := range counts.TextMadeValidUTF8 {
		fmt.Printf("not valid UTF-8, each broken sequence made U+FFFD: %s\n", value)
	}
	for _, table := range counts.TablesNotInTheFile {
		fmt.Printf("%-28s not in the file; left empty\n", table+":")
	}
	numbered := make([]string, 0, len(counts.NumberingContinuesAfter))
	for column := range counts.NumberingContinuesAfter {
		numbered = append(numbered, column)
	}
	sort.Strings(numbered)
	for _, column := range numbered {
		fmt.Printf("%s numbers on after %d\n", column, counts.NumberingContinuesAfter[column])
	}
	fmt.Printf("the archived events table handed out ids up to %d; the session log numbers above it; took %s\n", counts.HighestArchivedEventID, counts.Took.Round(1e6))
}
