// Command rule-store serves the rule store and runs its reactor: it follows
// llm-bridge-server's sessions and records what each active rule decides.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	rulestore "github.com/kayushkin/rule-store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("rule-store: ")
	settings, err := rulestore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("settings: %v", err)
	}
	store, err := rulestore.Open(settings.String(rulestore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	bridge := rulestore.NewLLMBridge(settings.String(rulestore.SettingLLMBridgeURL), settings.String(rulestore.SettingLLMBridgeServiceToken))
	kanban := rulestore.NewKanbanStore(settings.String(rulestore.SettingKanbanStoreURL), settings.String(rulestore.SettingKanbanStoreServiceToken))
	// A live run left applying by the last process stopped mid-way. Its calls
	// are not made again: the run says which answered, and a person decides.
	if interrupted, err := store.MarkInterruptedRuns(); err != nil {
		log.Fatalf("mark interrupted runs: %v", err)
	} else if interrupted > 0 {
		log.Printf("marked %d live runs interrupted: they were applying when the last process stopped", interrupted)
	}
	reactor := rulestore.NewReactor(store, bridge, kanban, rulestore.NewOwnerCalls(bridge, kanban), settings.Duration(rulestore.SettingSweepInterval))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go reactor.Run(ctx)

	mux := http.NewServeMux()
	rulestore.RegisterHandlers(ctx, mux, store, reactor, kanban)
	rulestore.RegisterSettingsHandler(mux, settings)
	server := &http.Server{Addr: settings.String(rulestore.SettingListenAddress), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
