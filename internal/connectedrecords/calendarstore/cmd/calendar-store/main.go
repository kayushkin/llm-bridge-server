// Command calendar-store serves calendar events, on
// 127.0.0.1:8342.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	calendarstore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/calendarstore"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := calendarstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(calendarstore.SettingListenAddress)
	store, err := calendarstore.Open(settings.String(calendarstore.SettingDataDirectory), calendarstore.NewPeopleStore(settings.String(calendarstore.SettingPeopleStoreURL)))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	calendarstore.RegisterHandlers(mux, store)
	calendarstore.RegisterSettingsHandler(mux, settings)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("calendar-store listening on %s (data=%s)", addr, store.DataDir())
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
