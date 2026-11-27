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

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictions"
)

func main() {
	// Subcommand dispatch — keep the default no-arg invocation as the
	// long-running server. Subcommands let admins do quick one-shot
	// operations against the local databases without going through HTTP.
	if len(os.Args) > 1 {
		if runBridgeOwnStoreImportCommand(os.Args[1], os.Args[2:]) {
			return
		}
		switch os.Args[1] {
		case "mint-enroll":
			mintEnrollCmd(os.Args[2:])
			return
		case "import-predictions":
			importPredictionStoreCmd(os.Args[2:])
			return
		case "import-tools":
			importToolStoreCmd(os.Args[2:])
			return
		case "import-skills":
			importSkillStoreCmd(os.Args[2:])
			return
		case "import-bundles":
			importBundleStoreCmd(os.Args[2:])
			return
		case "import-repos":
			importRepoStoreCmd(os.Args[2:])
			return
		case "import-git-activity":
			importWorkGraphStoreCmd(os.Args[2:])
			return
		case "import-files":
			importFileStoreCmd(os.Args[2:])
			return
		case "import-agents":
			importAgentStoreCmd(os.Args[2:])
			return
		case "import-memories":
			importMemoryStoreCmd(os.Args[2:])
			return
		case "import-models":
			importModelStoreCmd(os.Args[2:])
			return
		case "import-usage":
			importUsageStoreCmd(os.Args[2:])
			return
		case "import-evaluations":
			importEvalStoreCmd(os.Args[2:])
			return
		case "import-noteboard":
			importNoteboardCmd(os.Args[2:])
			return
		case "import-projects":
			importProjectStoreCmd(os.Args[2:])
			return
		case "import-issues":
			importKanbanStoreCmd(os.Args[2:])
			return
		case "import-session-log":
			importLogStoreCmd(os.Args[2:])
			return
		case "import-people", "import-calendar", "import-journal", "import-messages", "import-time-ledger":
			importConnectedRecordsStoreCmd(os.Args[1], os.Args[2:])
			return
		case "import-online-activity":
			importActivityStoreCmd(os.Args[2:])
			return
		case "import-photos":
			importPhotoStoreCmd(os.Args[2:])
			return
		case "import-locations":
			importLocationStoreCmd(os.Args[2:])
			return
		case "import-budget":
			importBudgetStoreCmd(os.Args[2:])
			return
		case "import-possessions":
			importPossessionStoreCmd(os.Args[2:])
			return
		case "import-mail":
			importMailstackCmd(os.Args[2:])
			return
		case "import-scheduler":
			importSchedulerCmd(os.Args[2:])
			return
		case "import-rules":
			importRuleStoreCmd(os.Args[2:])
			return
		case "import-bridge-database":
			importBridgeDatabaseCmd(os.Args[2:])
			return
		case "import-principals", "import-grants", "import-sign-in", "import-permissions":
			importAccessStoreCmd(os.Args[1], os.Args[2:])
			return
		case "make-sign-in-link":
			makeSignInLinkCmd(os.Args[2:])
			return
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "usage: llm-bridge-server [mint-enroll [-ttl 15m] | import-bridge-database -sqlite <file> | import-predictions -sqlite <file> | import-principals|import-grants|import-sign-in|import-permissions -sqlite <file> | make-sign-in-link <principal id> | import-noteboard -sqlite <file> | import-projects -sqlite <file> | import-issues -sqlite <file> | import-session-log -sqlite <file> | import-tools -sqlite <file> | import-skills -sqlite <file> | import-bundles -sqlite <file> | import-agents|import-memories|import-models -sqlite <file> | import-usage|import-evaluations -sqlite <file> | import-repos -sqlite <file> | import-git-activity -sqlite <file> | import-files -sqlite <file> -blobs <directory> [-carry copy|link] | import-people|import-calendar|import-journal|import-messages|import-time-ledger -sqlite <file> -owner <principal id> | import-online-activity -sqlite <file> -attachments <directory> -owner <principal id> | import-mail -accounts-sqlite <file> [-local-mail-sqlite <file>] | import-scheduler -sqlite <file> [-newest-runs-per-job <n>] | import-rules -sqlite <file> | import-photos -sqlite <file> -thumbnails <directory> -owner <principal id> [-carry copy|link] | import-locations -sqlite <file> -owner <principal id> | import-budget -sqlite <file> -owner <principal id> | import-possessions -sqlite <file> -originals <directory> -owner <principal id> [-carry copy|link] | import-machines|import-hooks|import-snapshots|import-operations -sqlite <file>]")
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
	log.Printf("every route is gated (service token or login session); principals, grants, sign-in and tool-call rules are the access stores in this process, and kanban-store is in this process too, at /issues/, all on Postgres")

	// Say where the bridge's own records are before writing to them: the
	// bridge_core module and the session event log (the session_log module),
	// both in the Postgres the URL names. An instance that isolates its data
	// folder and not LLMBRIDGE_POSTGRES_URL still writes into the shared
	// records, and AutoDiscover turns that into thousands of permanent writes
	// seconds after boot.
	log.Printf("bridge records: the bridge_core module | session event log: the session_log module | both in the Postgres of %s", config.PostgresURLEnvironmentVariable)

	st, err := openBridgeCoreStore(context.Background(), cfg.PostgresURL, cfg.DBPath)
	if err != nil {
		log.Fatalf("bridge core: %v", err)
	}
	defer st.Close()

	// agent-store, memory-store and model-store, in Postgres. Each is required.
	agentsRegistry := openAgentsRegistryModules(context.Background(), cfg)
	defer agentsRegistry.close()
	as, ms, mds := agentsRegistry.agents, agentsRegistry.memories, agentsRegistry.models

	// harness-store, hook-store, snapshot-store and operations, in Postgres.
	// Each is required.
	bridgeOwn := openBridgeOwnStoreModules(context.Background(), cfg)
	defer bridgeOwn.close()
	hs, hks, ss := bridgeOwn.harnesses, bridgeOwn.hooks, bridgeOwn.snapshots

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
	log.Printf("postgres: access open, served at /principals/, /grants/, /sign-in/ and /permissions/")

	// log-store: every session event. Nothing below may start or resume a
	// session before it is the harness manager's session event log.
	closeSessionLog, err := enableSessionLog(context.Background(), cfg, srv, st)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer closeSessionLog()

	// The modules that keep their records in Postgres. With no URL they are
	// off and their routes answer 503; with one, a module that cannot be
	// provisioned, migrated or opened stops the server here.
	if cfg.PostgresURL == "" {
		log.Printf("postgres: %s is not set, so prediction-store, notes, issues and project-store are off and their routes answer 503", config.PostgresURLEnvironmentVariable)
	} else {
		predictionStoreDB, err := openPredictionStoreModule(context.Background(), cfg.PostgresURL)
		if err != nil {
			log.Fatalf("prediction-store: %v", err)
		}
		defer predictionStoreDB.Close()
		srv.EnablePredictions(predictions.New(predictionStoreDB))
		log.Printf("postgres: prediction-store open, served at /predictions/")
		closeTaskTrackingModules, err := enableTaskTrackingModules(context.Background(), cfg, srv, accessStores)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeTaskTrackingModules()
		closeRepositoriesAndReferences, err := enableRepositoriesAndReferences(context.Background(), cfg, srv, accessStores)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeRepositoriesAndReferences()
		closeConnectedRecords, err := enableConnectedRecords(context.Background(), cfg, srv)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeConnectedRecords()
		closeMessageActivityAndTimeLedger, err := enableMessageActivityAndTimeLedgerStores(context.Background(), cfg, srv)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeMessageActivityAndTimeLedger()
		closePhotosAndLocations, err := enablePhotosAndLocations(context.Background(), cfg, srv)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closePhotosAndLocations()
		closeBudgetStore, err := enableBudgetStore(context.Background(), cfg, srv)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeBudgetStore()
		closeMail, err := enableMail(context.Background(), cfg, srv, accessStores)
		if err != nil {
			log.Fatalf("%v", err)
		}
		defer closeMail()
	}

	closeAgentsStoreModules := enableAgentsStoreModules(context.Background(), srv, cfg)
	defer closeAgentsStoreModules()

	// usage-store and eval-store, in Postgres. Their background loops run only
	// when their settings say so.
	closeUsageAndEvalStores, err := enableUsageAndEvalStores(context.Background(), cfg, srv, mds)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer closeUsageAndEvalStores()

	// The scheduler and rule-store, in Postgres. Neither runs a job or
	// decides a rule unless its setting says so.
	closeAutomation, err := enableAutomation(context.Background(), cfg, srv)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer closeAutomation()

	// possession-store, in Postgres, once model-store and the stores it asks
	// are open. Its review worker runs only when its settings say so.
	closePossessions, err := enablePossessions(context.Background(), cfg, srv)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer closePossessions()

	coordinator, err := operations.NewCoordinator(bridgeOwn.operations, operations.Config{
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
	log.Printf("operations: %d workers | lease %s", cfg.OperationsWorkerCount, cfg.OperationsLeaseDuration)
	srv.ReconcileAndResume() // Clean up stale running-state and resume recently-active sessions
	srv.RedeliverWakeupsLostInRestart()
	srv.DeliverMessagesHeldBeforeRestart()                        // Send what the previous process held while it drained for this restart
	srv.AutoDiscover()                                            // Import on-disk sessions from harnesses
	go srv.WatchExternalHarnessActivity(context.Background())     // Bring in terminal runs of claude and codex while the server is up
	srv.StartWatchdog()                                           // Periodic check for sessions whose harness died mid-life
	srv.StartPeerDialers()                                        // Reopen the socket of every dial-out connection this server initiated
	srv.Machines().StartMachineDiskUsage()                        // Record runners' finished deletions on the Machines page
	srv.Machines().StartMachineStatsSampler(context.Background()) // Read this machine every minute, keep runners' readings, delete old ones
	go srv.ChatLinks().Run(context.Background())                  // Deliver what people send linked conversations, and send the replies
	go func() {                                                   // Read messaging.inbound for chat links; a bus that is down is retried, not fatal to every session
		for {
			err := srv.ChatLinks().StartInbound(cfg.ChatLinksNATSURL)
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
	if cfg.PostgresURL == "" {
		fmt.Fprintf(os.Stderr, "error: %s is not set, so there is no harness-store to keep the enrollment in\n", config.PostgresURLEnvironmentVariable)
		os.Exit(1)
	}
	harnessStoreModule, err := machines.Module()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness-store: %v\n", err)
		os.Exit(1)
	}
	harnessStoreDB, err := openPostgresModule(context.Background(), cfg.PostgresURL, harnessStoreModule)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open harness-store: %v\n", err)
		os.Exit(1)
	}
	defer harnessStoreDB.Close()
	hs := machines.New(harnessStoreDB)

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
	fs := flag.NewFlagSet("import-predictions", flag.ExitOnError)
	sqlitePath := fs.String("sqlite", "", "the standalone prediction-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-predictions -sqlite <file>")
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
	counts, err := predictions.ImportStandaloneDatabase(ctx, db, *sqlitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: nothing was imported: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("predictions: %d (%d soft-deleted), last number %d\n", counts.Predictions, counts.DeletedPredictions, counts.LastPredictionSeq)
	fmt.Printf("estimates:   %d, last id %d\n", counts.Estimates, counts.LastEstimateID)
	fmt.Printf("links:       %d, last id %d\n", counts.Links, counts.LastLinkID)
}
