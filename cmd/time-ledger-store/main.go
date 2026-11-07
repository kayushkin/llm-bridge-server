// Command time-ledger-store serves the time ledger, on
// 127.0.0.1:8343.
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
	timeledgerstore "github.com/kayushkin/time-ledger-store"
)

func main() {
	settings, err := timeledgerstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(timeledgerstore.SettingListenAddress)
	store, err := timeledgerstore.Open(settings.String(timeledgerstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	timeledgerstore.RegisterHandlers(mux, store)
	timeledgerstore.RegisterSettingsHandler(mux, settings)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("time-ledger-store listening on %s (data=%s)", addr, store.DataDir())
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
