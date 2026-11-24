// Command tool-store runs the HTTP registry for harness-seedable tools.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	toolstore "github.com/kayushkin/tool-store"
	"github.com/kayushkin/tool-store/tools"
)

func main() {
	// Every environment variable the service reads is declared in settings.go.
	// A value that does not parse, or a set TOOL_STORE_ADDR… or
	// TOOL_STORE_DATA_DIR… variable nobody declared, stops the start here.
	settings, err := toolstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	addr := settings.String(toolstore.SettingListenAddress)

	// Before the registry of in-process tools is listed or seeded: browser,
	// web_search and scheduler are in it only once they are given where
	// PinchTab, Brave and the scheduler are.
	tools.RegisterToolsThatReachOutsideServices(toolstore.OutsideServiceConnectionsFrom(settings))

	store, err := toolstore.Open(settings.String(toolstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := seedLocalTools(store); err != nil {
		log.Fatalf("seed local tools: %v", err)
	}
	if err := seedMCPTools(store); err != nil {
		log.Fatalf("seed mcp tools: %v", err)
	}
	if err := seedHarnessTools(store); err != nil {
		log.Fatalf("seed harness tools: %v", err)
	}

	opts := toolstore.HandlerOptions{
		ResolveCredential: resolveFromAuthStore(settings.String(toolstore.SettingAuthStoreURL), settings.String(toolstore.SettingAuthStoreToken)),
		InvokeLocal: func(ctx context.Context, name, input string) (string, error) {
			impl, ok := tools.ByName(name)
			if !ok {
				return "", fmt.Errorf("local tool %q not registered", name)
			}
			return impl.Run(ctx, input)
		},
		ListLocals: func() []toolstore.LocalDescriptor {
			out := make([]toolstore.LocalDescriptor, 0, len(tools.All()))
			for _, impl := range tools.All() {
				schemaJSON, _ := json.Marshal(impl.InputSchema)
				out = append(out, toolstore.LocalDescriptor{
					Name:        impl.Name,
					Description: impl.Description,
					InputSchema: schemaJSON,
				})
			}
			return out
		},
	}

	mux := http.NewServeMux()
	toolstore.RegisterHandlers(mux, store, opts)
	toolstore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("tool-store listening on %s (data=%s)", addr, store.DataDir())
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

// seedLocalTools upserts every in-process registered tool as a kind=local row.
// New rows default to enabled=false — operators (or eventually bridge-ui) opt
// each tool in explicitly. Existing rows preserve their enabled state, so a
// user-enabled tool stays enabled across restarts and a re-disabled one stays
// disabled. Description and schema are always refreshed from code.
func seedLocalTools(store *toolstore.Store) error {
	for _, impl := range tools.All() {
		schemaJSON, err := json.Marshal(impl.InputSchema)
		if err != nil {
			return fmt.Errorf("marshal schema for %s: %w", impl.Name, err)
		}
		enabled := false
		if existing, err := store.GetToolByName(impl.Name); err == nil {
			enabled = existing.Enabled
		} else if !errors.Is(err, toolstore.ErrNotFound) {
			return fmt.Errorf("lookup local tool %s: %w", impl.Name, err)
		}
		t := &toolstore.Tool{
			Name:        impl.Name,
			Description: impl.Description,
			Kind:        toolstore.KindLocal,
			InputSchema: schemaJSON,
			Local:       &toolstore.LocalSpec{Symbol: impl.Name},
			Enabled:     enabled,
		}
		if _, err := store.UpsertTool(t); err != nil {
			return fmt.Errorf("upsert local tool %s: %w", impl.Name, err)
		}
	}
	return nil
}
