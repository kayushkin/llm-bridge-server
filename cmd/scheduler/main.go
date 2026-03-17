package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/slava/scheduler/internal/api"
	"github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
)

func main() {
	port := envOr("SCHEDULER_PORT", "8092")
	dbPath := envOr("SCHEDULER_DB", filepath.Join(os.Getenv("HOME"), ".scheduler", "scheduler.db"))
	token := os.Getenv("SCHEDULER_TOKEN")

	store, err := db.New(dbPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer store.Close()

	scheduler := cron.New(store)
	if err := scheduler.Start(); err != nil {
		log.Fatalf("cron: %v", err)
	}
	defer scheduler.Stop()

	srv := api.New(store, scheduler, token)

	go func() {
		addr := fmt.Sprintf(":%s", port)
		log.Printf("scheduler listening on %s", addr)
		if err := http.ListenAndServe(addr, srv); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
