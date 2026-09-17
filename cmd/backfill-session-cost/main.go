// backfill-session-cost raises the recorded cost (sessions.spend_usd) of Claude
// Code sessions to the per-process estimate over their stored events, for the
// events recorded before harnesses reported per-turn result costs.
//
// Until 2026-09-17 spend_usd was the per-call API sum alone, and before 2026-08-01
// it was not recorded at all, so thousands of sessions read $0 or less than they
// cost. The live derivation now keeps the estimate current; this computes it once
// for history. It only ever RAISES a recorded cost (RecordSessionCost is a MAX).
//
// -through-event-id is required: the last log-store event id recorded while
// harnesses still reported cumulative result costs (the id current when the
// per-turn harness change was deployed). Events after it are per-turn and are the
// live derivation's.
//
// Without -apply it only reports what it would change.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	_ "modernc.org/sqlite"
)

func main() {
	home, _ := os.UserHomeDir()
	logStoreDB := flag.String("log-store-db", filepath.Join(home, ".config", "log-store", "events.db"), "log-store events database, opened read-only")
	bridgeDB := flag.String("bridge-db", filepath.Join(home, ".llm-bridge", "bridge.db"), "llm-bridge-server database")
	throughEventID := flag.Int64("through-event-id", 0, "last log-store event id with cumulative result costs (required)")
	apply := flag.Bool("apply", false, "write the raised costs; without it, report only")
	flag.Parse()
	if *throughEventID <= 0 {
		log.Fatal("-through-event-id is required")
	}

	events, err := sql.Open("sqlite", "file:"+*logStoreDB+"?mode=ro")
	if err != nil {
		log.Fatalf("open log-store: %v", err)
	}
	defer events.Close()
	bridge, err := store.New(*bridgeDB)
	if err != nil {
		log.Fatalf("open bridge store: %v", err)
	}

	sessions, err := claudeCodeSessionCosts(*bridgeDB)
	if err != nil {
		log.Fatal(err)
	}

	var raised int
	var recordedUSD, estimatedUSD float64
	for _, sess := range sessions {
		evs, err := readCostEvents(events, sess.bridgeID, *throughEventID)
		if err != nil {
			log.Fatalf("read events of %s: %v", sess.bridgeID, err)
		}
		totalUSD, _, turnResultUSD := sessionCostEstimate(evs)
		recordedUSD += sess.spendUSD
		estimatedUSD += max(totalUSD, sess.spendUSD)
		if totalUSD <= sess.spendUSD+0.005 {
			continue
		}
		raised++
		if !*apply {
			continue
		}
		if _, err := bridge.RecordSessionCost(sess.bridgeID, totalUSD, turnResultUSD); err != nil {
			log.Fatalf("record cost of %s: %v", sess.bridgeID, err)
		}
	}
	verb := "would raise"
	if *apply {
		verb = "raised"
	}
	fmt.Printf("%d claude_code/jig sessions: recorded $%.2f, estimate $%.2f; %s %d\n", len(sessions), recordedUSD, estimatedUSD, verb, raised)
}

type sessionCost struct {
	bridgeID string
	spendUSD float64
}

// claudeCodeSessionCosts lists the sessions whose harnesses reported cumulative
// result costs, with their recorded cost.
func claudeCodeSessionCosts(bridgeDB string) ([]sessionCost, error) {
	db, err := sql.Open("sqlite", "file:"+bridgeDB+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT bridge_id, COALESCE(spend_usd, 0) FROM sessions WHERE harness IN ('claude_code', 'jig')`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []sessionCost
	for rows.Next() {
		var s sessionCost
		if err := rows.Scan(&s.bridgeID, &s.spendUSD); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func readCostEvents(db *sql.DB, sessionID string, throughEventID int64) ([]costEvent, error) {
	rows, err := db.Query(`SELECT type,
			COALESCE(CASE WHEN type='result' THEN json_extract(data, '$.result.cost.total_usd') END, 0),
			COALESCE(CASE WHEN type='api_call' THEN json_extract(data, '$.api_call.cost_usd') END, 0)
		FROM events WHERE session_id=? AND type IN ('result', 'api_call') AND id <= ? ORDER BY id`,
		sessionID, throughEventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []costEvent
	for rows.Next() {
		var typ string
		var ev costEvent
		if err := rows.Scan(&typ, &ev.ResultCumulativeUSD, &ev.CallUSD); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
