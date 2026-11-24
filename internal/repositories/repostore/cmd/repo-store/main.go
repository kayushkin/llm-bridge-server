// Command repo-store runs the HTTP registry of repositories and their detected
// signature tags.
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
	repostore "github.com/kayushkin/repo-store"
)

func main() {
	settings, err := repostore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(repostore.SettingListenAddress)

	store, err := repostore.Open(settings.String(repostore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	pullRequestSyncInterval := settings.Duration(repostore.SettingPullRequestSyncInterval)
	if pullRequestSyncInterval <= 0 {
		log.Fatalf("REPO_STORE_PULL_REQUEST_SYNC_INTERVAL must be positive, got %s", pullRequestSyncInterval)
	}
	github, err := repostore.NewGitHubCommandLine(settings.String(repostore.SettingGitHubCommandLineProgram))
	if err != nil {
		log.Fatalf("reach GitHub: %v", err)
	}
	pullRequestSyncer := repostore.NewPullRequestSyncer(store, github)

	mux := http.NewServeMux()
	repostore.RegisterHandlers(mux, store)
	repostore.RegisterPullRequestHandlers(mux, store, pullRequestSyncer)
	repostore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("repo-store listening on %s (data=%s)", addr, store.DataDir())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	syncContext, stopSync := context.WithCancel(context.Background())
	defer stopSync()
	go pullRequestSyncer.RunEvery(syncContext, pullRequestSyncInterval)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	stopSync()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
