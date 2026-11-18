package main

import (
	"log"
	"net/http"

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/log-store/internal/config"
	ls "github.com/kayushkin/log-store/internal/logstack"
	"github.com/kayushkin/log-store/internal/server"
	"github.com/kayushkin/log-store/internal/store"
)

func main() {
	settings, err := config.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("[log-store] read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("[log-store] read settings: %v", err)
	}
	cfg := config.Load(settings)

	s, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("[log-store] failed to open store: %v", err)
	}
	defer s.Close()

	forwarder := ls.NewForwarder(cfg.LogstackURL)
	// Probe logstack before serving. A URL pointing at the wrong service used
	// to be invisible until it had already dropped thousands of results, one
	// identical 404 at a time.
	forwarder.Preflight()
	srv := server.New(s, forwarder, settings)

	// Index the turns of every session written before the turn index existed,
	// newest first. Reads do not wait for it: a read that reaches an unindexed
	// session indexes that one itself.
	go func() {
		if err := srv.BackfillTurnIndex(); err != nil {
			log.Printf("[log-store] turn index backfill STOPPED: %v", err)
		}
	}()

	// Add the events stored before the token usage projections existed. It
	// reads for about a minute on a large database, so it runs beside the
	// service, not before it: llm-bridge-server drops an event it has failed
	// to push here for 30 seconds. Until it finishes, the aggregates of older
	// sessions are short of their cache tokens and API call totals.
	// One after the other: each reads a large part of the events table, and
	// two at once would only take turns at the disk.
	go func() {
		if _, err := s.BackfillTokenUsage(); err != nil {
			log.Printf("[log-store] token usage backfill STOPPED, older sessions' cache tokens and API call totals are missing until the next start: %v", err)
		}
		if _, err := s.BackfillUsageByHour(); err != nil {
			log.Printf("[log-store] hourly usage backfill STOPPED, a windowed aggregate is short of what happened before this start until the next one: %v", err)
		}
	}()

	log.Printf("[log-store] listening on %s (db: %s)", cfg.ListenAddr, cfg.DBPath)
	if err := http.ListenAndServe(cfg.ListenAddr, srv); err != nil {
		log.Fatalf("[log-store] server error: %v", err)
	}
}
