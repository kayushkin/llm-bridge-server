// Command memory-store runs the memory store as a standalone HTTP service.
//
// In production the memory-store library is mounted directly into
// llm-bridge-server, which calls memorystore.RegisterHandlers on its own mux
// and shares a single process (see llm-bridge-server internal/server/server.go).
// This standalone binary exposes the exact same HTTP surface on its own port.
// Its primary jobs are local development and giving the repo a from-source
// boot-and-answer entrypoint that the clean-checkout smoke guard
// (scripts/e2e-smoke.sh, scheduler job 27) can build and drive — a repo with no
// runnable entrypoint can compile green and still ship a binary that dies at
// boot (e.g. an http.ServeMux route-pattern panic no compiler catches).
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	memorystore "github.com/kayushkin/memory-store"
)

func main() {
	addr := os.Getenv("MEMORY_STORE_ADDR")
	if addr == "" {
		// Dev/standalone default. Production mounts the library into
		// llm-bridge-server on :8160 rather than running this binary, so this
		// port is not a deployed service address — override it via the env var.
		addr = ":8165"
	}

	dbPath := os.Getenv("MEMORY_STORE_DB")
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("resolve home dir for default db path: %v", err)
		}
		// Same canonical location llm-bridge-server defaults to
		// (internal/config/config.go: LLMBRIDGE_MEMORY_DB), so a standalone run
		// and the embedded run point at one DB unless told otherwise.
		dbPath = filepath.Join(home, ".config", "memory-store", "memory.db")
	}

	store, err := memorystore.NewStore(dbPath)
	if err != nil {
		log.Fatalf("open memory-store at %s: %v", dbPath, err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	// /health is not part of the library surface (RegisterHandlers only mounts
	// /memories/*); it lives here so the standalone binary has a cheap readiness
	// probe for the smoke and for any process supervisor.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
			log.Printf("health encode: %v", err)
		}
	})
	memorystore.RegisterHandlers(mux, store)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("memory-store listening on %s (db=%s)", addr, dbPath)
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
