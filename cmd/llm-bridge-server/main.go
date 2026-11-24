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

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/llm-bridge-server/internal/access"
	hookstore "github.com/kayushkin/llm-bridge-server/internal/agents/hookstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	harnessstore "github.com/kayushkin/llm-bridge-server/internal/machinemanagement/harnessstore"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	snapshotstore "github.com/kayushkin/llm-bridge-server/internal/sessions/snapshotstore"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictionstore"
	memorystore "github.com/kayushkin/memory-store"
	modelstore "github.com/kayushkin/model-store"
)

func main() {
	// Subcommand dispatch — keep the default no-arg invocation as the
	// long-running server. Subcommands let admins do quick one-shot
	// operations against the local databases without going through HTTP.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mint-enroll":
			mintEnrollCmd(os.Args[2:])
			return
		case "import-prediction-store":
			importPredictionStoreCmd(os.Args[2:])
			return
		case "import-tool-store":
			importToolStoreCmd(os.Args[2:])
			return
		case "import-skill-store":
			importSkillStoreCmd(os.Args[2:])
			return
		case "import-bundle-store":
			importBundleStoreCmd(os.Args[2:])
			return
		case "import-noteboard":
			importNoteboardCmd(os.Args[2:])
			return
		case "import-project-store":
			importProjectStoreCmd(os.Args[2:])
			return
		case "import-principal-store", "import-grant-store", "import-sign-in-store", "import-permission-store":
			importAccessStoreCmd(os.Args[1], os.Args[2:])
			return
		case "make-sign-in-link":
			makeSignInLinkCmd(os.Args[2:])
			return
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "usage: llm-bridge-server [mint-enroll [-ttl 15m] | import-prediction-store -sqlite <file> | import-principal-store|import-grant-store|import-sign-in-store|import-permission-store -sqlite <file> | make-sign-in-link <principal id> | import-noteboard -sqlite <file> | import-project-store -sqlite <file> | import-tool-store -sqlite <file> | import-skill-store -sqlite <file> | import-bundle-store -sqlite <file>]")
			os.Exit(0)
		}
	}

	cfg := config.Load()

	// Every request to this server is authorized before it reaches a handler,
	// so the credentials and store addresses that authorization needs are
	// mandatory. There is no half-configured state and no way to switch the
	// gate off: refuse to start instead.
	if err := cfg.ValidateRequestAuthorizationSettings(); err != nil {
		log.Fatalf("request authorization: %v", err)
	}
	// The same check, made from the declarations: every setting declared
	// Required is in force. It is a step of the server and not of config.Load,
	// because the operator commands of this binary run from a shell that has
	// none of them.
	if err := cfg.Settings.CheckRequired(); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	if err := forbidSameUserProcessesFromReadingThisProcess(); err != nil {
		log.Fatalf("refusing to start, because an agent running as this user could read the login signing key out of this process: %v", err)
	}
	// A half-configured Entra sign-in is a mistake in the unit; starting with
	// sign-in silently off would hide it until someone failed to sign in.
	if err := cfg.ValidateEntraSignInSettings(); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	if cfg.DemoLoginEnabled {
		log.Printf("demo login is ON: POST /auth/demo-login signs in as any active human principal-store principal with no password — a stand-in for real login, never expose it to the internet; LLMBRIDGE_DEMO_LOGIN_ENABLED=false switches it off")
	} else {
		log.Printf("demo login is OFF (LLMBRIDGE_DEMO_LOGIN_ENABLED=false): POST /auth/demo-login answers 404 demo_login_disabled")
	}
	if cfg.EntraSignInEnabled() {
		log.Printf("Entra ID sign-in is ON for tenant %s, client %s, issuer %s, redirect %s", cfg.EntraTenantID, cfg.EntraClientID, cfg.EntraIssuerURL(), cfg.EntraRedirectURL)
	} else {
		log.Printf("Entra ID sign-in is OFF: no LLMBRIDGE_ENTRA_* settings")
	}
	log.Printf("every route is gated (service token or login session); /kanban/ proxies to kanban-store %s carrying X-Principal-Id, so it may not be reachable except through here; principals, grants, sign-in and tool-call rules are the access stores in this process, on Postgres",
		cfg.KanbanStoreURL)

	// Say where the two stores that outlive this process are, before writing
	// to either. The local DB is per-instance and disposable; log-store is
	// remote, shared and permanent, and its URL has a live-host default, so
	// an instance that isolates LLMBRIDGE_DB_PATH and stops there is still
	// pointed at the production event log. Nothing used to say so: every
	// other store below logs where it loaded from, and log-store — the only
	// one whose writes cannot be thrown away with a temp directory — booted
	// silently. See AutoDiscover, which turns that into thousands of
	// permanent writes seconds after boot.
	log.Printf("bridge db %s | log-store %s (remote, shared, permanent — set LLMBRIDGE_LOG_STORE_URL to isolate an instance)",
		cfg.DBPath, cfg.LogStoreURL)

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

	// Initialize hook-store (optional)
	var hks *hookstore.Store
	if cfg.HookStoreDB != "" {
		hks, err = hookstore.Open(cfg.HookStoreDB)
		if err != nil {
			log.Printf("hook-store: %v (continuing without hook registry)", err)
			hks = nil
		} else {
			defer hks.Close()
			log.Printf("hook-store loaded from %s", cfg.HookStoreDB)
		}
	}

	// Initialize model-store — REQUIRED. It is the only source of a model:
	// every session's model is resolved through it (internal/server/
	// model_selection.go), so a server without it cannot start a session.
	// Refusing to start here is louder and earlier than refusing every
	// create later, and "continuing without model data" was how a server
	// could run for months with no idea what model any session was on.
	if cfg.ModelStoreDB == "" {
		log.Fatalf("model-store: no registry path configured (ModelStoreDB); the model registry is required")
	}
	mds, err := modelstore.Open(cfg.ModelStoreDB)
	if err != nil {
		log.Fatalf("model-store: open %s: %v", cfg.ModelStoreDB, err)
	}
	defer mds.Close()
	log.Printf("model-store loaded from %s", cfg.ModelStoreDB)

	// Initialize snapshot-store (optional)
	var ss *snapshotstore.Store
	if cfg.SnapshotStoreDB != "" && cfg.SnapshotStoreGit != "" {
		ss, err = snapshotstore.Open(snapshotstore.Config{
			DBPath: cfg.SnapshotStoreDB,
			GitDir: cfg.SnapshotStoreGit,
		})
		if err != nil {
			log.Printf("snapshot-store: %v (continuing without tool-call snapshots)", err)
			ss = nil
		} else {
			defer ss.Close()
			log.Printf("snapshot-store loaded from %s (git %s)", cfg.SnapshotStoreDB, cfg.SnapshotStoreGit)
		}
	}

	srv := server.New(st, as, ms, hs, hks, mds, ss, cfg)

	// access: who may sign in and what they may use. Every request's principal
	// is read from it, so it is not optional: a module that cannot be
	// provisioned, migrated or opened stops the server here.
	accessStores, err := access.OpenStores(context.Background(), cfg.PostgresURL)
	if err != nil {
		log.Fatalf("access: %v", err)
	}
	defer accessStores.Close()
	srv.EnableAccess(accessStores)
	log.Printf("postgres: access open, served at /principal-store/, /grant-store/, /sign-in-store/ and /permission-store/")

	// The modules that keep their records in Postgres. With no URL they are
	// off and their routes answer 503; with one, a module that cannot be
	// provisioned, migrated or opened stops the server here.
	if cfg.PostgresURL == "" {
		log.Printf("postgres: %s is not set, so prediction-store, noteboard and project-store are off and their routes answer 503", config.PostgresURLEnvironmentVariable)
	} else {
		predictionStoreDB, err := openPredictionStoreModule(context.Background(), cfg.PostgresURL)
		if err != nil {
			log.Fatalf("prediction-store: %v", err)
		}
		defer predictionStoreDB.Close()
		srv.EnablePredictionStore(predictionstore.New(predictionStoreDB))
		log.Printf("postgres: prediction-store open, served at /prediction-store/")
		closeTaskTrackingModules, err := enableTaskTrackingModules(context.Background(), cfg, srv, accessStores.Principals)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeTaskTrackingModules()
	}

	closeAgentsStoreModules := enableAgentsStoreModules(context.Background(), srv, cfg)
	defer closeAgentsStoreModules()

	operationStore, err := operationstore.Open(cfg.OperationsDBPath)
	if err != nil {
		log.Fatalf("operations db: %v", err)
	}
	defer operationStore.Close()
	coordinator, err := operations.NewCoordinator(operationStore, operations.Config{
		WorkerCount:   cfg.OperationsWorkerCount,
		LeaseDuration: cfg.OperationsLeaseDuration,
		// Unique per process, so a restart finds every operation the last
		// process left running and reconciles it.
		LeaseOwner:     fmt.Sprintf("llm-bridge-server pid %d started %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano)),
		ModelListPrice: srv.ModelListPrice,
	}, srv.OperationExecutors()...)
	if err != nil {
		log.Fatalf("operations: %v", err)
	}
	srv.EnableOperations(coordinator)
	operationsContext, stopOperations := context.WithCancel(context.Background())
	defer stopOperations()
	go coordinator.Run(operationsContext)
	log.Printf("operations db %s | %d workers | lease %s", cfg.OperationsDBPath, cfg.OperationsWorkerCount, cfg.OperationsLeaseDuration)
	srv.ReconcileAndResume() // Clean up stale running-state and resume recently-active sessions
	srv.RedeliverWakeupsLostInRestart()
	srv.DeliverMessagesHeldBeforeRestart()                    // Send what the previous process held while it drained for this restart
	srv.AutoDiscover()                                        // Import on-disk sessions from harnesses
	go srv.WatchExternalHarnessActivity(context.Background()) // Bring in terminal runs of claude and codex while the server is up
	srv.StartWatchdog()                                       // Periodic check for sessions whose harness died mid-life
	srv.StartPeerDialers()                                    // Reopen the socket of every dial-out connection this server initiated
	srv.StartMachineDiskUsage()                               // Record runners' finished deletions on the Machines page
	srv.StartMachineStatsSampler(context.Background())        // Read this machine every minute, keep runners' readings, delete old ones
	go srv.RunChatLinks(context.Background())                 // Deliver what people send linked conversations, and send the replies
	go func() {                                               // Read messaging.inbound for chat links; a bus that is down is retried, not fatal to every session
		for {
			err := srv.StartChatLinkInbound(cfg.ChatLinksNATSURL)
			if err == nil {
				return
			}
			log.Printf("[chat-links] ERROR: %v; trying again in a minute", err)
			time.Sleep(time.Minute)
		}
	}()
	go srv.RunPeerMessageOutbox(context.Background()) // Deliver queued messages to connected servers, and report finished updates
	stopKeepAwake := srv.StartKeepAwake()             // Hold this machine awake while sessions run, when keep_awake.enabled is on
	defer stopKeepAwake()

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

// mintEnrollCmd creates a single-use runner enrollment passphrase and
// prints it to stdout. The passphrase is the only output a user sees;
// only its hash is stored in harness-store. Designed to be wrapped by
// shell pipelines (e.g. piped into a registration QR code generator).
func mintEnrollCmd(args []string) {
	fs := flag.NewFlagSet("mint-enroll", flag.ExitOnError)
	ttl := fs.Duration("ttl", 15*time.Minute, "how long the passphrase is valid")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	cfg := config.Load()
	if cfg.HarnessStoreDB == "" {
		fmt.Fprintln(os.Stderr, "error: harness-store database not configured (set LLMBRIDGE_HARNESS_DB)")
		os.Exit(1)
	}
	hs, err := harnessstore.Open(cfg.HarnessStoreDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open harness-store: %v\n", err)
		os.Exit(1)
	}
	defer hs.Close()

	passphrase, enr, err := hs.MintEnrollment(*ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint enrollment: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("passphrase:  %s\n", passphrase)
	fmt.Printf("expires at:  %s (in %s)\n", enr.ExpiresAt.Local().Format(time.RFC3339), time.Until(enr.ExpiresAt).Round(time.Second))
	fmt.Printf("enrollment:  %s\n", enr.ID)
}

// importPredictionStoreCmd copies a standalone prediction-store's SQLite file
// into the Postgres module, keeping every id, number and time, and prints what
// it copied. It refuses a module that already holds any row. It reads the
// Postgres URL from the same setting the server does.
func importPredictionStoreCmd(args []string) {
	fs := flag.NewFlagSet("import-prediction-store", flag.ExitOnError)
	sqlitePath := fs.String("sqlite", "", "the standalone prediction-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-prediction-store -sqlite <file>")
		os.Exit(2)
	}
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		fmt.Fprintf(os.Stderr, "error: %s is not set, so there is no module to import into\n", config.PostgresURLEnvironmentVariable)
		os.Exit(1)
	}
	ctx := context.Background()
	db, err := openPredictionStoreModule(ctx, cfg.PostgresURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	counts, err := predictionstore.ImportStandaloneDatabase(ctx, db, *sqlitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: nothing was imported: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("predictions: %d (%d soft-deleted), last number %d\n", counts.Predictions, counts.DeletedPredictions, counts.LastPredictionSeq)
	fmt.Printf("estimates:   %d, last id %d\n", counts.Estimates, counts.LastEstimateID)
	fmt.Printf("links:       %d, last id %d\n", counts.Links, counts.LastLinkID)
}
