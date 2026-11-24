package main

import (
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/noteboard/internal/api"
	"github.com/kayushkin/noteboard/internal/db"
	"github.com/kayushkin/noteboard/internal/settings"
)

func main() {
	registry, err := settings.NewRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := registry.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	dbPath := registry.String(settings.DatabasePath)

	store, err := db.New(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	a := api.New(store, registry)
	addr := net.JoinHostPort(registry.String(settings.ListenHost), strconv.Itoa(registry.Integer(settings.ListenPort)))
	log.Printf("noteboard listening on %s (db: %s)", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, a.Handler()))
}
