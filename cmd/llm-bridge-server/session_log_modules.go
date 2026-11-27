package main

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog"
	logstack "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstack"
	sessionlogserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/server"
	sessionlogstore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/store"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// openSessionLogStore provisions, migrates and opens the session_log module
// (log-store) on the Postgres at url: its writer pool, and a read-only pool
// beside it.
func openSessionLogStore(ctx context.Context, url string, asynchronousCommit bool) (*sessionlogstore.Store, error) {
	module, err := sessionlogstore.Module()
	if err != nil {
		return nil, err
	}
	writerURL, err := sessionlogstore.WriterURL(url, asynchronousCommit)
	if err != nil {
		return nil, err
	}
	writer, err := openPostgresModule(ctx, writerURL, module)
	if err != nil {
		return nil, err
	}
	reader, err := sessionlogstore.OpenReader(ctx, url, module)
	if err != nil {
		writer.Close()
		return nil, err
	}
	return sessionlogstore.New(writer, reader), nil
}

// enableSessionLog opens log-store in Postgres and makes it this server's
// session event log, and the only copy of session events: its routes at
// /session-log/, the bridge's session routes answered from it, and every event
// the harness manager writes stored in it, in this process, with the
// session row or runner spawn the event describes written in the same
// transaction. It must run before any session starts or resumes. Result
// statistics go to logstack at session_log.logstack_url, or nowhere when that
// is empty. The returned function closes the pools.
func enableSessionLog(ctx context.Context, cfg *config.Config, srv *server.Server, bridge *store.Store) (func(), error) {
	sessionLogStore, err := openSessionLogStore(ctx, cfg.PostgresURL, cfg.Settings.Boolean(config.SettingSessionLogAsynchronousCommit))
	if err != nil {
		return nil, fmt.Errorf("log-store: %w", err)
	}
	module, err := sessionlogstore.Module()
	if err != nil {
		sessionLogStore.Close()
		return nil, err
	}
	if err := bridge.GrantSessionLogWrites(ctx, module.Role); err != nil {
		sessionLogStore.Close()
		return nil, err
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
	logServer := sessionlogserver.New(sessionLogStore, forwarder)
	srv.EnableSessionLog(logServer, logServer)
	log.Printf("postgres: log-store open, served at /session-log/ and as the session event log")
	singleCopy, err := sessionlog.OpenSingleCopy(ctx, bridge, logServer)
	if err != nil {
		sessionLogStore.Close()
		return nil, fmt.Errorf("session events: %w", err)
	}
	srv.UseSessionEventStore(singleCopy)
	numbering := singleCopy.Numbering()
	log.Printf("session events: the session log is the only copy, numbering from %d (the archived events table had handed out %d)",
		numbering.FirstEventID, numbering.BridgeDatabaseHighestEventID)

	// Repairs of the derived tables, beside the server's work and never before
	// it, as the standalone service ran them: each reads every session.
	go func() {
		rebuilt, err := sessionLogStore.RebuildMissingSessionRows()
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
		if err := sessionLogStore.Close(); err != nil {
			log.Printf("postgres: closing log-store: %v", err)
		}
	}, nil
}

// importLogStoreCmd copies a standalone log-store's SQLite file into the
// session_log module, keeping every event id, body and time and every derived
// table, and prints what it copied. It refuses a module that already holds
// any row.
func importLogStoreCmd(args []string) {
	sqlitePath := importArguments("import-log-store", "log-store", args)
	ctx := context.Background()
	module, err := sessionlogstore.Module()
	if err != nil {
		importFailed(err)
	}
	db, err := openPostgresModule(ctx, importPostgresURL(), module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := sessionlogstore.ImportStandaloneDatabase(ctx, db, sqlitePath)
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
