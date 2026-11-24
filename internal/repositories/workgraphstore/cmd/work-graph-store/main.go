package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	workgraphstore "github.com/kayushkin/work-graph-store"
)

func main() {
	settings, err := workgraphstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(workgraphstore.SettingListenAddress)

	store, err := workgraphstore.Open(settings.String(workgraphstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	repoStore := &workgraphstore.RepoStoreClient{BaseURL: settings.String(workgraphstore.SettingRepoStoreURL), HTTP: &http.Client{Timeout: 10 * time.Second}}

	ingestContext, stopIngesting := context.WithCancel(context.Background())
	defer stopIngesting()
	ingester := &workgraphstore.Ingester{
		Store:       store,
		SpoolPath:   settings.String(workgraphstore.SettingSpoolPath),
		RepoStore:   repoStore,
		Interval:    2 * time.Second,
		SettleDelay: 200 * time.Millisecond,
	}
	go ingester.Run(ingestContext)

	mux := http.NewServeMux()
	workgraphstore.RegisterHandlers(mux, &workgraphstore.Handlers{Store: store, RepoStore: repoStore})
	workgraphstore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("work-graph-store listening on %s (data=%s, spool=%s)", addr, store.DataDirectory(), ingester.SpoolPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	stopIngesting()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
