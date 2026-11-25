// Command budget-store serves the operator's finance ledger on 127.0.0.1:8340.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	budgetstore "github.com/kayushkin/budget-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := budgetstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(budgetstore.SettingListenAddress)
	store, err := budgetstore.Open(settings.String(budgetstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if environment := settings.String(budgetstore.SettingPlaidEnvironment); environment != "" {
		authURL, authToken := settings.String(budgetstore.SettingAuthStoreURL), settings.String(budgetstore.SettingAuthStoreToken)
		if authURL == "" || authToken == "" {
			log.Fatalf("BUDGET_STORE_PLAID_ENVIRONMENT is %q but BUDGET_STORE_AUTH_STORE_URL or BUDGET_STORE_AUTH_STORE_TOKEN is unset: Plaid's keys live in auth-store", environment)
		}
		store.SetPlaidVault(budgetstore.AuthStoreVault{BaseURL: authURL, Token: authToken, PlaidEnvironment: environment, Client: &http.Client{Timeout: 30 * time.Second}})
		log.Printf("plaid on, environment %s", environment)
	}

	mux := http.NewServeMux()
	budgetstore.RegisterHandlers(mux, store)
	budgetstore.RegisterSettingsHandler(mux, settings)

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("budget-store listening on %s (data=%s)", addr, store.DataDir())
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
