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

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/mailstack"
	"github.com/kayushkin/mailstack/accounts"
	"github.com/kayushkin/mailstack/apiauth"
	"github.com/kayushkin/mailstack/filestore"
)

func main() {
	// Read before the flags are declared, because three flags default to a
	// setting, and before any database is opened: a misspelled MAILSTACK_
	// variable stops the service here, with nothing touched.
	settings, err := mailstack.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}

	port := flag.Int("port", settings.Integer(mailstack.SettingListenPort), "HTTP listen port (loopback)")
	dbPath := flag.String("db", settings.String(mailstack.SettingAccountsDatabasePath), "accounts.db path (default: ~/.config/mailstack/accounts.db)")
	localDBPath := flag.String("local-db", settings.String(mailstack.SettingLocalMailDatabasePath),
		"synthetic mail db for local accounts (default: ~/.config/mailstack/local-mail.db, created only when a local account is used)")
	flag.Parse()

	if *dbPath == "" {
		log.Fatalf("no accounts database: the home directory is unknown, so there is no default; set MAILSTACK_DB or pass --db")
	}

	authStoreURL := settings.String(mailstack.SettingAuthStoreURL)
	if settings.String(mailstack.SettingBearerToken) == "" {
		log.Println("WARNING: MAILSTACK_TOKEN not set — /api/* and /settings are unauthenticated")
	}

	store, err := accounts.Open(*dbPath)
	if err != nil {
		log.Fatalf("open accounts db: %v", err)
	}
	defer store.Close()

	auth := apiauth.New(authStoreURL, settings.String(mailstack.SettingAuthStoreBearerToken))
	options := []mailstack.ServerOption{mailstack.WithLocalMailDB(*localDBPath)}
	if fileStoreURL := settings.String(mailstack.SettingFileStoreURL); fileStoreURL != "" {
		fileStoreToken := settings.String(mailstack.SettingFileStoreServiceToken)
		if fileStoreToken == "" {
			log.Fatalf("FILE_STORE_URL is set but FILE_STORE_SERVICE_TOKEN is not: file-store takes no call without it")
		}
		options = append(options, mailstack.WithFileStore(filestore.New(fileStoreURL, fileStoreToken)))
	} else {
		log.Println("FILE_STORE_URL not set — imports with attachments and attachment reads on local accounts answer 503")
	}
	srv := mailstack.NewServer(store, auth, settings, options...)

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
