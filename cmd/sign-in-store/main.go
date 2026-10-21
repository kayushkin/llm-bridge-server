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
	signinstore "github.com/kayushkin/sign-in-store"
)

func main() {
	settings, err := signinstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(signinstore.SettingListenAddress)

	store, err := signinstore.Open(settings.String(signinstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	directory := signinstore.NewDirectory(signinstore.DirectoryConfig{
		PrincipalStoreURL:     settings.String(signinstore.SettingPrincipalStoreURL),
		PeopleStoreURL:        settings.String(signinstore.SettingPeopleStoreURL),
		MultichatURL:          settings.String(signinstore.SettingMultichatURL),
		AuthStoreURL:          settings.String(signinstore.SettingAuthStoreURL),
		AuthStoreToken:        settings.String(signinstore.SettingAuthStoreToken),
		MultichatCredentialID: settings.String(signinstore.SettingMultichatCredential),
	})

	mux := http.NewServeMux()
	signinstore.RegisterHandlers(mux, store, directory, signinstore.ServerConfig{
		ServiceToken:  settings.String(signinstore.SettingServiceToken),
		SignInPageURL: settings.String(signinstore.SettingSignInPageURL),
		CookieDomain:  settings.String(signinstore.SettingCookieDomain),
	})
	signinstore.RegisterSettingsHandler(mux, settings)

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("sign-in-store listening on %s (data=%s)", addr, store.DataDir())
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
