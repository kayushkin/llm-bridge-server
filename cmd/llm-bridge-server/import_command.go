package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// importArguments reads an import command's one flag, the SQLite file to copy.
func importArguments(command, store string, args []string) string {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone "+store+" database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "usage: llm-bridge-server %s -sqlite <file>\n", command)
		os.Exit(2)
	}
	return *sqlitePath
}

// importPostgresURL is the Postgres an import copies into: the server's own
// setting.
func importPostgresURL() string {
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		fmt.Fprintf(os.Stderr, "error: %s is not set, so there is no module to import into\n", config.PostgresURLEnvironmentVariable)
		os.Exit(1)
	}
	return cfg.PostgresURL
}

func importFailed(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
