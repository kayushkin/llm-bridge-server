// Command activity-store serves what the user did online, on
// 127.0.0.1:8336.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	activitystore "github.com/kayushkin/activity-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := activitystore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(activitystore.SettingListenAddress)
	store, err := activitystore.Open(settings.String(activitystore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	activitystore.RegisterHandlers(mux, store)
	activitystore.RegisterSettingsHandler(mux, settings)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("activity-store listening on %s (data=%s)", addr, store.DataDir())
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
