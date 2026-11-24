// Command journal-store serves the user's own writing on 127.0.0.1:8322.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	journalstore "github.com/kayushkin/journal-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// registryRefreshInterval is how often reference-store's list of reference types is
// read again. A type added there reaches new mentions within this long, and
// old mentions are backfilled on the same tick.
const registryRefreshInterval = 5 * time.Minute

func main() {
	settings, err := journalstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(journalstore.SettingListenAddress)

	registry := journalstore.NewReferenceStoreEntityTypeRegistry(settings.String(journalstore.SettingReferenceStoreURL))
	noteboard := journalstore.NewNoteboardClient(settings.String(journalstore.SettingNoteboardURL))

	store, err := journalstore.Open(settings.String(journalstore.SettingDataDirectory), registry, noteboard)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	refreshRegistry := func() {
		if err := registry.Refresh(); err != nil {
			log.Printf("ERROR entity-type registry: %v — new mentions are stored untyped until a read succeeds; GET /health says degraded", err)
			return
		}
		typed, err := store.BackfillMentionTypes()
		if err != nil {
			log.Printf("ERROR backfill mention types: %v", err)
			return
		}
		if typed > 0 {
			log.Printf("typed %d mentions stored before the registry could classify them", typed)
		}
	}
	refreshRegistry()
	go func() {
		ticker := time.NewTicker(registryRefreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			refreshRegistry()
		}
	}()

	mux := http.NewServeMux()
	journalstore.RegisterHandlers(mux, store)
	journalstore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("journal-store listening on %s (data=%s)", addr, store.DataDir())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
