// Command bundle-store runs the HTTP registry of curated session bundles and
// the /resolve composition endpoint.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	bundlestore "github.com/kayushkin/bundle-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	// checkMembers is the pre-flight the deploy script runs BEFORE it stops the
	// running service. A bundle stores each member as the id skill-store or
	// tool-store assigned, and either store can delete or rename that row at any
	// time without telling bundle-store. This flag reads every stored bundle and
	// asks both stores about every member id, so a member that points at nothing
	// is found by a deploy rather than by a session that silently lacks it.
	checkMembers := flag.Bool("check-members", false,
		"check every member of every stored bundle against skill-store and tool-store by id, report, and exit")
	flag.Parse()

	// The registry is built before the check-members branch, not after it: that
	// pre-flight opens the same data directory the server does and asks
	// skill-store and tool-store, so it needs all three settings, and a deploy
	// should learn about a misspelled BUNDLE_STORE_ variable from the pre-flight
	// rather than from a service that will not come back up.
	settings, err := bundlestore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}

	if *checkMembers {
		if err := runMemberCheck(settings); err != nil {
			log.Fatalf("check-members: %v", err)
		}
		return
	}

	addr := settings.String(bundlestore.SettingListenAddress)

	store, err := bundlestore.Open(settings.String(bundlestore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if store.QuarantinedMembers > 0 {
		log.Printf("MIGRATION: %d bundle member(s) were keyed by name and carry no id; "+
			"they have been moved to table %s and are no longer resolved. "+
			"Re-add any hand-curated ones through POST /bundles with the id its owning store assigned.",
			store.QuarantinedMembers, bundlestore.LegacyMembersTable)
	}
	if store.BackfilledExtendsLinks > 0 {
		log.Printf("MIGRATION: %d bundle inheritance link(s) named their parent by name; "+
			"each has been converted to that parent's id and the extends column is gone. "+
			"Renaming a parent bundle no longer strands its children.",
			store.BackfilledExtendsLinks)
	}

	mux := http.NewServeMux()
	bundlestore.RegisterHandlers(mux, store)
	bundlestore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("bundle-store listening on %s (data=%s)", addr, store.DataDir())
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
