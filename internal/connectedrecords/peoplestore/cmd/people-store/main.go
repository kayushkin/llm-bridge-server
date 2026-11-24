// Command people-store serves the registry of the people in the operator's
// life on 127.0.0.1:8321.
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
	peoplestore "github.com/kayushkin/people-store"
)

func main() {
	settings, err := peoplestore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(peoplestore.SettingListenAddress)

	principals := peoplestore.NewHTTPPrincipalChecker(settings.String(peoplestore.SettingPrincipalStoreURL))
	store, err := peoplestore.Open(settings.String(peoplestore.SettingDataDirectory), principals)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	peoplestore.RegisterHandlers(mux, store)
	peoplestore.RegisterSettingsHandler(mux, settings)

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("people-store listening on %s (data=%s)", addr, store.DataDir())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
