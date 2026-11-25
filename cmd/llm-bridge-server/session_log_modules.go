package main

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	logstack "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/logstack"
	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	logstorestore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// openSessionLogStore provisions, migrates and opens the session_log module
// (log-store) on the Postgres at url: its writer pool, and a read-only pool
// beside it.
func openSessionLogStore(ctx context.Context, url string) (*logstorestore.Store, error) {
	module, err := logstorestore.Module()
	if err != nil {
		return nil, err
	}
	writer, err := openPostgresModule(ctx, url, module)
	if err != nil {
		return nil, err
	}
	reader, err := logstorestore.OpenReader(ctx, url, module)
	if err != nil {
		writer.Close()
		return nil, err
	}
	return logstorestore.New(writer, reader), nil
}

// enableSessionLog opens log-store in Postgres and makes it this server's
// session event log: its routes at /log-store/, the bridge's session routes
// answered from it, and every event the harness manager writes stored in it,
// in this process. It must run before any session starts or resumes. Result
// statistics go to logstack at session_log.logstack_url, or nowhere when that
// is empty. The returned function closes the pools.
//
// session_log.single_copy decides where every event is stored and read back:
// on, in the session log only; off, in the bridge database's events table,
// with a copy queued to the session log.
func enableSessionLog(ctx context.Context, cfg *config.Config, srv *server.Server, bridge *store.Store) (func(), error) {
	store, err := openSessionLogStore(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("log-store: %w", err)
	}
	var forwarder *logstack.Forwarder
	if logstackURL := cfg.Settings.String(config.SettingSessionLogLogstackURL); logstackURL == "" {
		log.Printf("log-store: %s is empty, so no result statistics go to logstack", config.SettingSessionLogLogstackURL)
	} else {
		forwarder = logstack.NewForwarder(logstackURL)
		// Probe logstack before serving: a URL pointing at the wrong service
		// is otherwise invisible until it has dropped thousands of results.
		forwarder.Preflight()
	}
	logServer := logstoreserver.New(store, forwarder)
	srv.EnableSessionLog(logServer, logServer)
	log.Printf("postgres: log-store open, served at /log-store/ and as the session event log")
	if err := chooseSessionEventStore(ctx, cfg, srv, bridge, logServer); err != nil {
		store.Close()
		return nil, err
	}

	// Repairs of the derived tables, beside the server's work and never before
	// it, as the standalone service ran them: each reads every session.
	go func() {
		rebuilt, err := store.RebuildMissingSessionRows()
		if err != nil {
			log.Printf("[log-store] ERROR: rebuilding missing sessions rows STOPPED: %v", err)
		} else if rebuilt > 0 {
			log.Printf("[log-store] rebuilt %d missing sessions rows from their events", rebuilt)
		}
		if err := logServer.BackfillTurnIndex(); err != nil {
			log.Printf("[log-store] ERROR: turn index backfill STOPPED: %v", err)
		}
	}()
	return func() {
		if err := store.Close(); err != nil {
			log.Printf("postgres: closing log-store: %v", err)
		}
	}, nil
}

// chooseSessionEventStore names where the harness manager stores events and
// the server reads them back, by session_log.single_copy.
func chooseSessionEventStore(ctx context.Context, cfg *config.Config, srv *server.Server, bridge *store.Store, logServer *logstoreserver.Server) error {
	if cfg.Settings.Boolean(config.SettingSessionLogSingleCopy) {
		singleCopy, err := sessionevents.OpenSingleCopy(ctx, bridge, logServer)
		if err != nil {
			return fmt.Errorf("session events: %w", err)
		}
		srv.UseSessionEventStore(singleCopy, true)
		numbering := singleCopy.Numbering()
		log.Printf("session events: the session log is the only copy, numbering from %d (the bridge database's events table had handed out %d); the bridge database's events table is no longer written",
			numbering.FirstEventID, numbering.BridgeDatabaseHighestEventID)
		return nil
	}
	// The old path after the session log was the only copy: the bridge
	// database numbers on above every id the session log handed out, so no
	// status or cursor a client holds outranks what it writes next.
	if _, wasSingleCopy, err := logServer.Store().SingleCopyNumberingRecord(); err != nil {
		return fmt.Errorf("session events: read the session log's numbering record: %w", err)
	} else if wasSingleCopy {
		highest, err := logServer.Store().HighestEventID()
		if err != nil {
			return fmt.Errorf("session events: %w", err)
		}
		if err := bridge.RaiseEventNumberingAbove(highest); err != nil {
			return fmt.Errorf("session events: raise the bridge database's numbering above %d: %w", highest, err)
		}
		log.Printf("session events: the session log was the only copy before; the bridge database's events table numbers on above %d", highest)
	}
	srv.UseSessionEventStore(sessionevents.BridgeDatabase{Store: bridge}, false)
	log.Printf("session events: stored in the bridge database's events table and queued to the session log (%s is off)", config.SettingSessionLogSingleCopy)
	return nil
}

// importLogStoreCmd copies a standalone log-store's SQLite file into the
// session_log module, keeping every event id, body and time and every derived
// table, and prints what it copied. It refuses a module that already holds
// any row.
func importLogStoreCmd(args []string) {
	sqlitePath := importArguments("import-log-store", "log-store", args)
	ctx := context.Background()
	module, err := logstorestore.Module()
	if err != nil {
		importFailed(err)
	}
	db, err := openPostgresModule(ctx, importPostgresURL(), module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := logstorestore.ImportStandaloneDatabase(ctx, db, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	tables := make([]string, 0, len(counts.RowsByTable))
	for table := range counts.RowsByTable {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		fmt.Printf("%-32s %d\n", table+":", counts.RowsByTable[table])
	}
	fmt.Printf("events number on after %d; took %s\n", counts.LastEventNumber, counts.Took.Round(1e6))
}
