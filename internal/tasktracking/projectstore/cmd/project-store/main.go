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
	projectstore "github.com/kayushkin/llm-bridge-server/internal/tasktracking/projectstore"
)

func main() {
	settings, err := projectstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("settings: %v", err)
	}
	addr := settings.String(projectstore.SettingListenAddress)

	store, err := projectstore.Open(settings.String(projectstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	owners := &projectstore.Owners{
		HTTP:                    &http.Client{Timeout: 10 * time.Second},
		RepoStoreURL:            settings.String(projectstore.SettingRepoStoreURL),
		KanbanStoreURL:          settings.String(projectstore.SettingKanbanStoreURL),
		KanbanStoreServiceToken: settings.String(projectstore.SettingKanbanStoreServiceToken),
		SchedulerURL:            settings.String(projectstore.SettingSchedulerURL),
		HealthcheckURL:          settings.String(projectstore.SettingHealthcheckURL),
		PrincipalStoreURL:       settings.String(projectstore.SettingPrincipalStoreURL),
		NoteboardURL:            settings.String(projectstore.SettingNoteboardURL),
		WorkGraphStoreURL:       settings.String(projectstore.SettingWorkGraphStoreURL),
		BridgeURL:               settings.String(projectstore.SettingBridgeURL),
		BridgeServiceToken:      settings.String(projectstore.SettingBridgeServiceToken),
		SuggestionModelRole:     settings.String(projectstore.SettingSuggestionModelRole),
		ModelHTTP:               &http.Client{Timeout: 5 * time.Minute},
	}

	mux := http.NewServeMux()
	sessionFiler := &projectstore.SessionFiler{
		Store:                          store,
		Owners:                         owners,
		ModelRole:                      settings.String(projectstore.SettingSessionFilerModelRole),
		MinimumConfidence:              settings.Decimal(projectstore.SettingSessionFilerMinimumConfidence),
		QuietBeforeFilingWithoutABrief: settings.Duration(projectstore.SettingSessionFilerQuietBeforeFilingWithoutABrief),
		Window:                         settings.Duration(projectstore.SettingSessionFilerWindow),
		SessionsPerPass:                settings.Integer(projectstore.SettingSessionFilerSessionsPerPass),
	}
	if err := sessionFiler.Validate(); err != nil {
		log.Fatalf("settings: %v", err)
	}
	projectstore.RegisterHandlers(mux, &projectstore.Handlers{Store: store, Owners: owners, SessionFiler: sessionFiler})
	projectstore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("project-store listening on %s", addr)
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
