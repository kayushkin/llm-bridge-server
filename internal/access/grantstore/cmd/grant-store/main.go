// Command grant-store serves the grant registry on 127.0.0.1:8315.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	grantstore "github.com/kayushkin/grant-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := grantstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(grantstore.SettingListenAddress)

	store, err := grantstore.Open(settings.String(grantstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// The owners a grant is checked against before it is written: the
	// principal with principal-store, the resource with whichever store
	// assigned its id.
	principalStoreURL := settings.String(grantstore.SettingPrincipalStoreURL)
	llmBridgeServerURL := settings.String(grantstore.SettingLLMBridgeServerURL)
	skillStoreURL := settings.String(grantstore.SettingSkillStoreURL)
	toolStoreURL := settings.String(grantstore.SettingToolStoreURL)
	kanbanStoreURL := settings.String(grantstore.SettingKanbanStoreURL)
	kanbanStoreServiceToken := settings.String(grantstore.SettingKanbanStoreServiceToken)
	llmBridgeServiceToken := settings.String(grantstore.SettingLLMBridgeServiceToken)
	directory := grantstore.NewHTTPPrincipalDirectory(principalStoreURL)
	checker := grantstore.NewHTTPResourceChecker(grantstore.ResourceOwners{
		LLMBridgeServerURL:         llmBridgeServerURL,
		LLMBridgeServiceToken:      llmBridgeServiceToken,
		PrincipalStoreURL:          principalStoreURL,
		SkillStoreURL:              skillStoreURL,
		ToolStoreURL:               toolStoreURL,
		KanbanStoreURL:             kanbanStoreURL,
		KanbanStoreServiceToken:    kanbanStoreServiceToken,
		ReferenceStoreURL:          settings.String(grantstore.SettingReferenceStoreURL),
		ReferenceStoreServiceToken: settings.String(grantstore.SettingReferenceStoreServiceToken),
	})
	log.Printf("owners: principal-store=%s llm-bridge-server=%s skill-store=%s tool-store=%s kanban-store=%s (kanban-store token set: %t, llm-bridge-server token set: %t)",
		principalStoreURL, llmBridgeServerURL, skillStoreURL, toolStoreURL, kanbanStoreURL, kanbanStoreServiceToken != "", llmBridgeServiceToken != "")

	mux := http.NewServeMux()
	serviceToken := settings.String(grantstore.SettingServiceToken)
	if len(serviceToken) < 32 {
		log.Fatal("GRANT_STORE_SERVICE_TOKEN must be at least 32 characters: it is what an internal service presents, and without it every request that omits the header would be unrestricted")
	}
	enforcement := grantstore.PrincipalEnforcement{ServiceToken: serviceToken}
	grantstore.RegisterHandlers(mux, store, directory, checker, enforcement)
	grantstore.RegisterSettingsHandler(mux, settings, directory, enforcement)
	log.Printf("every request needs X-Principal-Id (set by the gateway from a login) or the service token; an administrator is unrestricted")

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("grant-store listening on %s (data=%s)", addr, store.DataDir())
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
