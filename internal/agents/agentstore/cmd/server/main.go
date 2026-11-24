package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/agent-store/internal/config"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := config.NewServerSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	scanInterval, err := autoScanInterval(settings)
	if err != nil {
		log.Fatal(err)
	}
	addr := settings.String(config.SettingListenAddress)
	dbPath := settings.String(config.SettingDatabasePath)

	store, err := agentstore.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	agentstore.RegisterHandlers(mux, store)
	// This process owns its mux, so it serves its own /health. Hosts that
	// embed agent-store must not -- see RegisterHealthHandler.
	agentstore.RegisterHealthHandler(mux)
	// Every route here is open, so this one is too; nothing it describes is
	// Editable, so PUT is not mounted.
	mux.Handle("GET /settings", servicesettings.Handler(settings, "/settings"))

	if scanInterval > 0 {
		go runAutoScanner(store, scanInterval)
	}

	log.Printf("agent-store listening on %s (db: %s, auto-scan=%s)", addr, dbPath, scanInterval)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// autoScanInterval is AGENT_STORE_SCAN_INTERVAL_SECS as a duration; 0 turns
// the scan off. A negative value is refused: it used to be logged and replaced
// with the default, which ran a scan nobody had asked for.
func autoScanInterval(settings *servicesettings.Registry) (time.Duration, error) {
	seconds := settings.Integer(config.SettingAutoScanIntervalInSeconds)
	if seconds < 0 {
		return 0, fmt.Errorf("AGENT_STORE_SCAN_INTERVAL_SECS is %d; it must be 0 (no scan) or a number of seconds", seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

// runAutoScanner periodically re-walks $HOME so out-of-band edits to tracked
// files (someone touching ~/CLAUDE.md from a terminal, a git pull on a repo
// with AGENTS.md, etc.) are picked up without the user clicking Scan in dash.
// Each scan that detects a hash change appends a scan-import version row, so
// nothing is silently lost between scans.
func runAutoScanner(store *agentstore.Store, interval time.Duration) {
	// Stagger the first run so a fresh start doesn't compete with handler
	// traffic. After that, fire on the interval.
	time.Sleep(30 * time.Second)
	for {
		res, err := store.Scan()
		if err != nil {
			log.Printf("auto-scan: %v", err)
		} else if res.Added > 0 || res.Updated > 0 || res.Missing > 0 || len(res.Errors) > 0 {
			// This condition is a noise gate: a scan that found nothing must
			// say nothing, or the interesting runs are unfindable. It only
			// works because Updated counts content changes. While Updated
			// meant "the row already existed" the gate was true on every run
			// -- measured 2026-08-08, 2641 of this service's 2657 journal
			// lines were this message reporting zero real work, one every 15
			// minutes since 2026-07-11, with the handful of genuine 1-new and
			// 2-missing runs buried among them.
			//
			// Errors joined the gate because a run that dropped a file is
			// exactly an interesting run, and it is the only reader that never
			// sees the JSON the HTTP handler returns. Scanned is an undercount
			// whenever the last number is nonzero, so the line says so rather
			// than leaving a reader to notice the arithmetic.
			log.Printf("auto-scan: %d scanned (%d new, %d updated, %d unchanged, %d missing, %d failed)",
				res.Scanned, res.Added, res.Updated, res.Unchanged, res.Missing, len(res.Errors))
			// One line per failure: a count tells nobody which file went
			// unrecorded, and that is the question the scan was asked.
			for _, e := range res.Errors {
				log.Printf("auto-scan: unaccounted %s (%s): %s", e.Path, e.Stage, e.Err)
			}
		}
		time.Sleep(interval)
	}
}
