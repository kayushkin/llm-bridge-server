package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kayushkin/mailstack"
	"github.com/kayushkin/mailstack/accounts"
	"github.com/kayushkin/mailstack/apiauth"
)

func main() {
	port := flag.Int("port", envInt("MAILSTACK_PORT", 8195), "HTTP listen port (loopback)")
	dbPath := flag.String("db", os.Getenv("MAILSTACK_DB"), "accounts.db path (default: ~/.config/mailstack/accounts.db)")
	localDBPath := flag.String("local-db", os.Getenv("MAILSTACK_LOCAL_DB"),
		"synthetic mail db for local accounts (default: ~/.config/mailstack/local-mail.db, created only when a local account is used)")
	flag.Parse()

	if *dbPath == "" {
		*dbPath = accounts.DefaultDBPath()
	}

	authStoreURL := envOr("AUTH_STORE_URL", "http://127.0.0.1:8303")
	authStoreToken := os.Getenv("AUTH_STORE_TOKEN")
	mailstackToken := os.Getenv("MAILSTACK_TOKEN")
	if mailstackToken == "" {
		log.Println("WARNING: MAILSTACK_TOKEN not set — API is unauthenticated")
	}

	store, err := accounts.Open(*dbPath)
	if err != nil {
		log.Fatalf("open accounts db: %v", err)
	}
	defer store.Close()

	auth := apiauth.New(authStoreURL, authStoreToken)
	srv := mailstack.NewServer(store, auth, mailstackToken, mailstack.WithLocalMailDB(*localDBPath))

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		httpSrv.Shutdown(ctx)
	}()

	log.Printf("mailstack listening on %s (db=%s, auth-store=%s)", addr, *dbPath, authStoreURL)
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}
