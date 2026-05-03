package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	permstore "github.com/kayushkin/permission-store"
	"github.com/kayushkin/permission-store/internal/server"
)

func main() {
	port := flag.Int("port", 8304, "HTTP listen port")
	dbPath := flag.String("db", "", "SQLite path (default: ~/.config/permission-store/permission-store.db)")
	flag.Parse()

	store, err := permstore.Open(*dbPath)
	if err != nil {
		log.Fatalf("[permission-store] open store: %v", err)
	}
	defer store.Close()

	srv := server.New(store)
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[permission-store] listening on %s (db %s)", addr, store.Path())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[permission-store] serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("[permission-store] shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("[permission-store] shutdown: %v", err)
	}
}
