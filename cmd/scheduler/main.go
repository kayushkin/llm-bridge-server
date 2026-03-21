package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	natsbus "github.com/kayushkin/bus"
	"github.com/slava/scheduler/internal/api"
	"github.com/slava/scheduler/internal/bus"
	"github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
	"github.com/slava/scheduler/internal/logging"
)

func main() {
	port := envOr("SCHEDULER_PORT", "8092")
	dbPath := envOr("SCHEDULER_DB", filepath.Join(os.Getenv("HOME"), ".scheduler", "scheduler.db"))
	token := os.Getenv("SCHEDULER_TOKEN")
	natsURL := envOr("NATS_URL", "nats://localhost:4222")

	// Connect to NATS (optional — if it fails, we run without bus/logging)
	var natsClient *natsbus.Client
	nc, err := natsbus.Connect(natsbus.Options{URL: natsURL, Name: "scheduler"})
	if err != nil {
		log.Printf("warning: NATS connect failed (running without bus): %v", err)
	} else {
		natsClient = nc
	}

	// Initialize logging (uses NATS client, nil-safe)
	logger := logging.New(natsClient)
	logger.LogSchedulerLifecycle("starting")

	// Initialize database
	store, err := db.New(dbPath)
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to initialize database: %v", err), nil)
		log.Fatalf("db: %v", err)
	}
	defer store.Close()

	// Initialize scheduler
	scheduler := cron.New(store)
	scheduler.SetLogger(logger)

	// Wire up agent executor if NATS is available
	if natsClient != nil {
		agentExec := executor.NewAgentExecutor(natsClient)
		scheduler.SetAgentExecutor(agentExec)
		log.Println("agent executor configured (NATS)")
	}

	// Initialize bus handler (reuses the NATS connection URL; "" means use env)
	var natsURLForBus string
	if natsClient != nil {
		// Pass empty so bus.NewHandler sees natsURL="" and we pass client via different path
		// Actually, bus.NewHandler connects independently. Let's pass the URL.
		natsURLForBus = natsURL
	}
	busHandler, err := bus.NewHandler(natsURLForBus, store, scheduler, logger)
	if err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to initialize bus handler: %v", err), nil)
		log.Fatalf("bus: %v", err)
	}
	scheduler.SetBusHandler(busHandler)

	// Start scheduler
	if err := scheduler.Start(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to start scheduler: %v", err), nil)
		log.Fatalf("cron: %v", err)
	}
	defer func() {
		scheduler.Stop()
		busHandler.Close()
		if natsClient != nil {
			natsClient.Close()
		}
		logger.LogSchedulerLifecycle("stopped")
	}()

	// Start bus handler
	if err := busHandler.Start(); err != nil {
		logger.LogError("startup_error", fmt.Sprintf("Failed to start bus handler: %v", err), nil)
		log.Printf("warning: bus handler failed to start: %v", err)
	}

	// Initialize API server
	srv := api.New(store, scheduler, token)
	srv.SetLogger(logger)

	// Start HTTP server
	go func() {
		addr := fmt.Sprintf(":%s", port)
		log.Printf("scheduler listening on %s", addr)
		logger.LogSchedulerLifecycle("started", fmt.Sprintf("listening on %s", addr))
		if err := http.ListenAndServe(addr, srv); err != nil {
			logger.LogError("http_error", fmt.Sprintf("HTTP server error: %v", err), nil)
			log.Fatalf("http: %v", err)
		}
	}()

	// Wait for shutdown signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	logger.LogSchedulerLifecycle("shutting_down")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
