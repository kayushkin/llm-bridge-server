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
	harnessstore "github.com/kayushkin/harness-store"
	hookstore "github.com/kayushkin/hook-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	memorystore "github.com/kayushkin/memory-store"
	modelstore "github.com/kayushkin/model-store"
	snapshotstore "github.com/kayushkin/snapshot-store"
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
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "usage: llm-bridge-server [mint-enroll [-ttl 15m]]")
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
	log.Printf("every route is gated (service token or login); POST /auth/demo-login signs in as any active human principal-store principal with no password — a stand-in for real login, never expose it to the internet; /kanban/ proxies to kanban-store %s and /grant-store/ to grant-store %s carrying X-Principal-Id, so neither store may be reachable except through here; principals read from principal-store %s",
		cfg.KanbanStoreURL, cfg.GrantStoreURL, cfg.PrincipalStoreURL)

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
	srv.AutoDiscover()       // Import on-disk sessions from harnesses
	srv.StartWatchdog()      // Periodic check for sessions whose harness died mid-life

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
