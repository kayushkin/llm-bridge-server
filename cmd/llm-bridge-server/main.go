package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	agentstore "github.com/kayushkin/agent-store"
	harnessstore "github.com/kayushkin/harness-store"
	memorystore "github.com/kayushkin/memory-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("bridge db: %v", err)
	}
	defer st.Close()

	// Initialize agent-store (optional)
	var as *agentstore.Store
	if cfg.AgentStoreDB != "" {
		as, err = agentstore.Open(cfg.AgentStoreDB)
		if err != nil {
			log.Printf("agent-store: %v (continuing without agent data)", err)
			as = nil
		} else {
			defer as.Close()
			log.Printf("agent-store loaded from %s", cfg.AgentStoreDB)
		}
	}

	// Initialize memory-store (optional)
	var ms *memorystore.Store
	if cfg.MemoryStoreDB != "" {
		ms, err = memorystore.NewStore(cfg.MemoryStoreDB)
		if err != nil {
			log.Printf("memory-store: %v (continuing without memory data)", err)
			ms = nil
		} else {
			defer ms.Close()
			log.Printf("memory-store loaded from %s", cfg.MemoryStoreDB)
		}
	}

	// Initialize harness-store (optional)
	var hs *harnessstore.Store
	if cfg.HarnessStoreDB != "" {
		hs, err = harnessstore.Open(cfg.HarnessStoreDB)
		if err != nil {
			log.Printf("harness-store: %v (continuing without instance data)", err)
			hs = nil
		} else {
			defer hs.Close()
			log.Printf("harness-store loaded from %s", cfg.HarnessStoreDB)
		}
	}

	srv := server.New(st, as, ms, hs)

	go func() {
		log.Printf("llm-bridge-server listening on %s", cfg.ListenAddr)
		if err := http.ListenAndServe(cfg.ListenAddr, srv); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nshutting down")
}
