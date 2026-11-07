// Command possession-store serves the inventory of the things a person owns,
// on 127.0.0.1:8341, and reviews queued photos in the background.
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
	possessionstore "github.com/kayushkin/possession-store"
)

func main() {
	settings, err := possessionstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	workerEnabled := settings.Boolean(possessionstore.SettingReviewWorkerEnabled)
	if workerEnabled && settings.String(possessionstore.SettingBridgeServiceToken) == "" {
		log.Fatalf("read settings: the review worker is on but LLMBRIDGE_SERVICE_TOKEN is not set; set it, or set POSSESSION_STORE_REVIEW_WORKER_ENABLED=false")
	}
	httpClient := &http.Client{Timeout: 15 * time.Minute}
	references := possessionstore.ServiceReferences{
		PhotoStoreURL:        settings.String(possessionstore.SettingPhotoStoreURL),
		LocationStoreURL:     settings.String(possessionstore.SettingLocationStoreURL),
		HarnessStoreDatabase: settings.String(possessionstore.SettingHarnessStoreDatabase),
		HTTPClient:           &http.Client{Timeout: 30 * time.Second},
	}
	store, err := possessionstore.Open(settings.String(possessionstore.SettingDataDirectory), references)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	reviewer := &possessionstore.Reviewer{
		Store: store,
		Models: &possessionstore.BridgeModelCaller{
			BridgeURL:     settings.String(possessionstore.SettingBridgeURL),
			ServiceToken:  settings.String(possessionstore.SettingBridgeServiceToken),
			ModelStoreURL: settings.String(possessionstore.SettingModelStoreURL),
			HTTPClient:    httpClient,
		},
		Photos: possessionstore.PhotoStoreSource{
			PhotoStoreURL: settings.String(possessionstore.SettingPhotoStoreURL),
			OriginalsDir:  store.OriginalsDir(),
			LongestSide:   settings.Integer(possessionstore.SettingReviewImageLongestSide),
			HTTPClient:    &http.Client{Timeout: time.Minute},
		},
		FirstReaderRole:  settings.ModelRole(possessionstore.SettingFirstReaderRole),
		SecondReaderRole: settings.ModelRole(possessionstore.SettingSecondReaderRole),
	}
	ownerLocation, err := time.LoadLocation(settings.String(possessionstore.SettingOwnerTimeZone))
	if err != nil {
		log.Fatalf("read settings: POSSESSION_STORE_OWNER_TIME_ZONE: %v", err)
	}
	reviewer.OwnerLocation = ownerLocation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if workerEnabled {
		go reviewer.Run(ctx)
	} else {
		log.Printf("the review worker is off; queued photos wait")
	}

	mux := http.NewServeMux()
	(&possessionstore.Server{Store: store, Reviewer: reviewer}).RegisterHandlers(mux)
	possessionstore.RegisterSettingsHandler(mux, settings)
	address := settings.String(possessionstore.SettingListenAddress)
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("possession-store listening on %s (data=%s)", address, store.DataDir())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	cancel()
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
