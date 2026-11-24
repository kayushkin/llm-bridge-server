// Command reference-store serves the reference types and looks ids up in the
// stores that own them, on 127.0.0.1:8343.
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
	referencestore "github.com/kayushkin/reference-store"
)

func main() {
	settings, err := referencestore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	serviceURLs := settings.StringMap(referencestore.SettingServiceURLs)
	siteURLs := settings.StringMap(referencestore.SettingSiteURLs)
	if err := referencestore.ValidateReferenceConfiguration(referencestore.Types, serviceURLs, siteURLs); err != nil {
		log.Fatal(err)
	}
	seconds := func(key string) time.Duration { return time.Duration(settings.Integer(key)) * time.Second }
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	accessTTL := seconds(referencestore.SettingAccessCacheSeconds)
	bridgeHostedServices := map[string]bool{}
	for _, service := range settings.StringList(referencestore.SettingBridgeHostedServices) {
		bridgeHostedServices[service] = true
	}
	if len(bridgeHostedServices) > 0 && settings.String(referencestore.SettingBridgeServiceToken) == "" {
		log.Fatalf("%s names services llm-bridge-server serves, and %s is not set, so every call to them would be refused",
			"REFERENCE_STORE_BRIDGE_HOSTED_SERVICES", "REFERENCE_STORE_BRIDGE_SERVICE_TOKEN")
	}
	credentials := &referencestore.Credentials{
		BridgeServiceToken:   settings.String(referencestore.SettingBridgeServiceToken),
		BridgeHostedServices: bridgeHostedServices,
		AuthStoreURL:         settings.String(referencestore.SettingAuthStoreURL),
		AuthStoreToken:       settings.String(referencestore.SettingAuthStoreToken),
		AuthStoreProviders:   settings.StringMap(referencestore.SettingAuthStoreProviders),
		HTTPClient:           client,
		TTL:                  5 * time.Minute,
	}
	resolver := &referencestore.Resolver{
		Types:       referencestore.Types,
		ServiceURLs: serviceURLs,
		SiteURLs:    siteURLs,
		Credentials: credentials,
		Access: &referencestore.AccessChecker{
			PrincipalStoreURL: settings.String(referencestore.SettingPrincipalStoreURL),
			GrantStoreURL:     settings.String(referencestore.SettingGrantStoreURL),
			Credentials:       credentials,
			HTTPClient:        client,
			TTL:               accessTTL,
		},
		HTTPClient:    client,
		RecordTTL:     seconds(referencestore.SettingRecordCacheSeconds),
		MissTTL:       seconds(referencestore.SettingMissCacheSeconds),
		LookupTimeout: seconds(referencestore.SettingLookupTimeoutSeconds),
		Concurrency:   settings.Integer(referencestore.SettingLookupConcurrencyPerAsk),
	}

	mux := http.NewServeMux()
	referencestore.RegisterHandlers(mux, &referencestore.Server{
		Resolver:     resolver,
		ServiceToken: settings.String(referencestore.SettingServiceToken),
	})
	referencestore.RegisterSettingsHandler(mux, settings)
	addr := settings.String(referencestore.SettingListenAddress)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("reference-store listening on %s (%d types)", addr, len(referencestore.Types))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
