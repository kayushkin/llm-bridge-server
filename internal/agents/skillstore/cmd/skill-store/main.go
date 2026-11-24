// Command skill-store runs the HTTP registry for Claude Code / Agent skills.
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
	skillstore "github.com/kayushkin/skill-store"
)

func main() {
	settings, err := skillstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(skillstore.SettingListenAddress)

	store, err := skillstore.Open(settings.String(skillstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Where installed skills are copied. With nothing set the setting carries
	// DefaultInstallDir(), which is what Open already put on the store; an
	// override is what lets a test drive the install/uninstall/seed routes
	// without writing into the live skills directory.
	store.SetInstallDir(settings.String(skillstore.SettingInstallDirectory))

	mux := http.NewServeMux()
	skillstore.RegisterHandlers(mux, store)
	skillstore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("skill-store listening on %s (data=%s install=%s)", addr, store.DataDir(), store.InstallDir())
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
	_ = srv.Shutdown(ctx)
}
