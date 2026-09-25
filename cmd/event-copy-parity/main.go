// event-copy-parity compares the two copies of each session's events: the one
// llm-bridge-server keeps in bridge.db and the one it pushes to log-store. Both
// are handed the same serialized msg.Event, so for every session they should
// hold the same bodies, the same number of times.
//
// It is step 1 of the merge plan in noteboard note
// 15d4ca97-b076-4a91-9386-1d8863b4f417: before bridge.db's events table can be
// retired, the two copies must be shown to agree, and this is what shows it.
//
// It compares the sessions the bridge stored an event for in the last -window
// and that have been quiet for -settle in both stores. Sessions that exist only
// in log-store are out of scope: those are transcripts imported from disk (the
// oneshot calls), which never pass through bridge.db.
//
// Both databases are opened read-only. It prints a count per event class of
// what only one copy holds, then up to -show differing sessions with their
// events. It exits 0 when the copies agree, 1 when they do not or the check
// could not run.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	_ "modernc.org/sqlite"
)

// sqliteTimestampLayout is how SQLite's CURRENT_TIMESTAMP writes created_at.
const sqliteTimestampLayout = "2006-01-02 15:04:05"

func main() {
	bridgeDatabasePath := flag.String("bridge-db", productiondefaults.BridgeDatabasePath(), "llm-bridge-server database, opened read-only")
	logStoreDatabasePath := flag.String("log-store-db", "", "log-store events database, opened read-only (required: log-store owns its path, LOG_STORE_DB_PATH)")
	window := flag.Duration("window", 24*time.Hour, "compare sessions the bridge stored an event for within this long")
	settle := flag.Duration("settle", 10*time.Minute, "leave out a session with an event this recent in either store; log-store is written from a queue")
	show := flag.Int("show", 20, "list at most this many differing sessions in full")
	flag.Parse()
	if *logStoreDatabasePath == "" {
		log.Fatal("-log-store-db is required")
	}
	if *settle <= 0 || *window <= *settle {
		log.Fatalf("-window (%s) must be longer than -settle (%s), and -settle positive", *window, *settle)
	}

	bridge, err := openReadOnly(*bridgeDatabasePath)
	if err != nil {
		log.Fatalf("open bridge database: %v", err)
	}
	defer bridge.Close()
	logStore, err := openReadOnly(*logStoreDatabasePath)
	if err != nil {
		log.Fatalf("open log-store database: %v", err)
	}
	defer logStore.Close()

	now := time.Now().UTC()
	windowStart := now.Add(-*window).Format(sqliteTimestampLayout)
	settledBefore := now.Add(-*settle).Format(sqliteTimestampLayout)

	sessionIDs, err := sessionsQuietInWindow(bridge, windowStart, settledBefore)
	if err != nil {
		log.Fatal(err)
	}

	var differing []sessionComparison
	onlyInBridgeByClass := map[string]int{}
	onlyInLogStoreByClass := map[string]int{}
	var compared, stillWritingToLogStore, bridgeEventCount, logStoreEventCount int
	for _, sessionID := range sessionIDs {
		logStoreLatest, err := latestEventCreatedAt(logStore, sessionID)
		if err != nil {
			log.Fatal(err)
		}
		if logStoreLatest >= settledBefore {
			stillWritingToLogStore++
			continue
		}
		bridgeEvents, err := readSessionEvents(bridge, sessionID)
		if err != nil {
			log.Fatal(err)
		}
		logStoreEvents, err := readSessionEvents(logStore, sessionID)
		if err != nil {
			log.Fatal(err)
		}
		compared++
		bridgeEventCount += len(bridgeEvents)
		logStoreEventCount += len(logStoreEvents)
		comparison := compareSessionCopies(sessionID, bridgeEvents, logStoreEvents)
		if !comparison.differs() {
			continue
		}
		differing = append(differing, comparison)
		for _, ev := range comparison.OnlyInBridge {
			onlyInBridgeByClass[eventClass(ev)]++
		}
		for _, ev := range comparison.OnlyInLogStore {
			onlyInLogStoreByClass[eventClass(ev)]++
		}
	}

	fmt.Printf("window %s .. %s UTC: %d sessions compared (%d bridge events, %d log-store events); %d left out, log-store still writing\n",
		windowStart, settledBefore, compared, bridgeEventCount, logStoreEventCount, stillWritingToLogStore)
	if len(differing) == 0 {
		fmt.Println("parity: every compared session holds the same events in both stores")
		return
	}
	fmt.Printf("DIFFER: %d of %d sessions\n", len(differing), compared)
	printClassCounts("only in bridge.db", onlyInBridgeByClass)
	printClassCounts("only in log-store", onlyInLogStoreByClass)
	for i, comparison := range differing {
		if i == *show {
			fmt.Printf("\n... and %d more sessions (raise -show)\n", len(differing)-*show)
			break
		}
		fmt.Printf("\n%s: bridge.db %d, log-store %d\n", comparison.SessionID, comparison.BridgeCount, comparison.LogStoreCount)
		for _, ev := range comparison.OnlyInBridge {
			fmt.Printf("  only in bridge.db  row %d  %s  %s\n", ev.RowID, ev.CreatedAt, eventClass(ev))
		}
		for _, ev := range comparison.OnlyInLogStore {
			fmt.Printf("  only in log-store  row %d  %s  %s\n", ev.RowID, ev.CreatedAt, eventClass(ev))
		}
	}
	os.Exit(1)
}

func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func printClassCounts(heading string, countByClass map[string]int) {
	if len(countByClass) == 0 {
		fmt.Printf("  %s: none\n", heading)
		return
	}
	classes := make([]string, 0, len(countByClass))
	for class := range countByClass {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool {
		if countByClass[classes[i]] != countByClass[classes[j]] {
			return countByClass[classes[i]] > countByClass[classes[j]]
		}
		return classes[i] < classes[j]
	})
	parts := make([]string, len(classes))
	for i, class := range classes {
		parts[i] = fmt.Sprintf("%s %d", class, countByClass[class])
	}
	fmt.Printf("  %s: %s\n", heading, strings.Join(parts, ", "))
}
