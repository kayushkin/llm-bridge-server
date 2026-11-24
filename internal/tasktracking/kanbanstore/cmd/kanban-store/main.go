package main

import (
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/filestore"
	"github.com/kayushkin/kanban-store/internal/grantstore"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/multichat"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"github.com/kayushkin/kanban-store/internal/principalstore"
	"github.com/kayushkin/kanban-store/internal/referencestore"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := config.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	cfg, err := config.Load(settings)
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}

	store, err := db.New(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	nb := noteboard.New(cfg.NoteboardURL)
	a := api.New(store, nb, principalstore.New(cfg.PrincipalStoreURL), llmbridge.New(cfg.LLMBridgeServerURL), bundlestore.New(cfg.BundleStoreURL), settings)
	a.SetReferenceStore(referencestore.New(cfg.ReferenceStoreURL))

	// Attachments live in file-store. Without it this store has none, and the
	// attachment routes answer 503 rather than report that a card has no files.
	// config.Load has already refused a URL with no token beside it.
	if cfg.FileStoreURL != "" {
		a.SetFileStore(filestore.New(cfg.FileStoreURL, cfg.FileStoreServiceToken))
		log.Printf("attachments: files are kept in file-store at %s", cfg.FileStoreURL)
	} else {
		log.Printf("attachments: FILE_STORE_URL is not set; the attachment routes answer 503")
	}

	// Message triggers send through multichat only when MULTICHAT_URL is set.
	// Without it they still match and render, and each delivery is recorded as
	// not_configured, so the board's settings page shows them firing into
	// nothing rather than silently doing nothing.
	if cfg.MultichatURL != "" {
		a.SetMessageSender(multichat.New(cfg.MultichatURL, multichat.AuthStoreTokenSource(cfg.AuthStoreURL, cfg.AuthStoreToken)))
		log.Printf("message triggers: delivering through multichat at %s (token from auth-store %s)", cfg.MultichatURL, cfg.AuthStoreURL)
	} else {
		log.Printf("message triggers: MULTICHAT_URL is not set; matching triggers are recorded as not_configured and nothing is sent")
	}

	a.SetPrincipalEnforcement(api.PrincipalEnforcement{
		ServiceToken: cfg.ServiceToken,
		Grants:       grantstore.New(cfg.GrantStoreURL, cfg.GrantStoreServiceToken),
	})
	log.Printf("every request needs X-Principal-Id (set by the gateway from a login) or the service token; board grants come from grant-store at %s, and an administrator is past every check", cfg.GrantStoreURL)

	addr := net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.ListenPort))
	log.Printf("kanban-store listening on %s (db: %s, noteboard: %s, principal-store: %s, llm-bridge-server: %s, bundle-store: %s)",
		addr, cfg.DatabasePath, cfg.NoteboardURL, cfg.PrincipalStoreURL, cfg.LLMBridgeServerURL, cfg.BundleStoreURL)
	log.Fatal(http.ListenAndServe(addr, a.Handler()))
}
