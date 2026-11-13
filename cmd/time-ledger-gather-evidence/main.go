// Command time-ledger-gather-evidence links to each time entry the records
// that show where the operator was while it ran, and what it cost:
// location-store visits that overlap it, photo-store photos taken during it,
// and the spending budget-store keeps on a trip, linked to the entry the trip
// is the source of. Visits and photos bear on the entry as context: the
// reviewer, not this program, decides whether they show the entry happened.
// A trip charge bears on it as cost once budget-store has it confirmed, and as
// context while it is only suggested (see gatherTripSpending).
//
// It reads entries that have ended and last at most -longest (a journey's
// visits are the trip itself), widens each span by -margin on both sides,
// since people arrive late and leave early, and looks -instant-window past
// the start of an entry with no end. It never links again what the operator
// took off.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"time"

	timeledgerstore "github.com/kayushkin/time-ledger-store"
	"github.com/kayushkin/time-ledger-store/internal/storeclient"
)

const signer = "time-ledger-gather-evidence"

type visit struct {
	ID        string `json:"id"`
	PlaceName string `json:"place_name"`
	Locality  *struct {
		City   string `json:"city"`
		Region string `json:"region"`
	} `json:"locality"`
	StartedAt int64 `json:"started_at"`
	EndedAt   int64 `json:"ended_at"`
	DeletedAt int64 `json:"deleted_at"`
}

type photo struct {
	ID      string `json:"id"`
	TakenAt int64  `json:"taken_at"`
}

func main() {
	ledgerURL := flag.String("time-ledger-store-url", "http://127.0.0.1:8344", "where time-ledger-store answers")
	locationStoreURL := flag.String("location-store-url", "http://127.0.0.1:8333", "where location-store answers")
	photoStoreURL := flag.String("photo-store-url", "http://127.0.0.1:8329", "where photo-store answers")
	budgetStoreURL := flag.String("budget-store-url", "http://127.0.0.1:8340", "where budget-store answers")
	days := flag.Int("days", 60, "read entries that ended in the last this many days")
	longest := flag.Duration("longest", 24*time.Hour, "skip entries longer than this")
	margin := flag.Duration("margin", 30*time.Minute, "how far before and after an entry to look")
	instantWindow := flag.Duration("instant-window", 3*time.Hour, "how far past the start of an entry with no end to look")
	flag.Parse()

	now := time.Now()
	from := now.AddDate(0, 0, -*days).Unix()
	linked, entriesRead := 0, 0
	for offset := 0; ; offset += timeledgerstore.MaxListLimit {
		var entries []timeledgerstore.TimeEntry
		query := url.Values{"from": {fmt.Sprint(from)}, "ended_before": {fmt.Sprint(now.Unix())}, "limit": {fmt.Sprint(timeledgerstore.MaxListLimit)},
			"offset": {fmt.Sprint(offset)}}
		if err := storeclient.Get(*ledgerURL+"/time-entries?"+query.Encode(), &entries); err != nil {
			log.Fatal(err)
		}
		for _, entry := range entries {
			if time.Duration(entry.EndsAt-entry.StartsAt)*time.Second > *longest {
				continue
			}
			entriesRead++
			lookFrom, lookTo := entry.StartsAt-int64(margin.Seconds()), entry.EndsAt+int64(margin.Seconds())
			window := fmt.Sprintf("within %s of it", margin)
			if entry.EndsAt == entry.StartsAt {
				lookTo = entry.StartsAt + int64(instantWindow.Seconds())
				window = fmt.Sprintf("from %s before it starts to %s after (it has no end)", margin, instantWindow)
			}
			made, err := gather(*ledgerURL, *locationStoreURL, *photoStoreURL, entry, lookFrom, lookTo, window)
			if err != nil {
				log.Fatal(err)
			}
			linked += made
		}
		if len(entries) < timeledgerstore.MaxListLimit {
			break
		}
	}
	log.Printf("read %d entries; %d links added", entriesRead, linked)
	changes, err := gatherTripSpending(*ledgerURL, *budgetStoreURL)
	if err != nil {
		log.Fatal(err)
	}
	logSpendingChanges(changes)
}

func gather(ledgerURL, locationStoreURL, photoStoreURL string, entry timeledgerstore.TimeEntry, lookFrom, lookTo int64, window string) (int, error) {
	made := 0
	var visits []visit
	query := url.Values{"from": {fmt.Sprint(lookFrom)}, "to": {fmt.Sprint(lookTo)}}
	if err := storeclient.Get(locationStoreURL+"/visits?"+query.Encode(), &visits); err != nil {
		return 0, err
	}
	for _, place := range visits {
		if place.DeletedAt != 0 || place.EndedAt < lookFrom || place.StartedAt > lookTo {
			continue
		}
		overlap := min(place.EndedAt, entry.EndsAt) - max(place.StartedAt, entry.StartsAt)
		if overlap < 0 {
			overlap = 0
		}
		name := place.PlaceName
		if name == "" {
			name = "an unnamed place"
		}
		town := "town not named"
		if place.Locality != nil {
			town = place.Locality.City + ", " + place.Locality.Region
		}
		reason := fmt.Sprintf("the operator was at %s (%s) %s", name, town, window)
		ok, err := link(ledgerURL, entry.ID, map[string]any{"ref": place.ID, "overlap_seconds": overlap, "bears_on": timeledgerstore.BearsOnContext, "reason": reason}, "visits")
		if err != nil {
			return made, err
		}
		if ok {
			made++
		}
	}
	var photos []photo
	query = url.Values{"from": {fmt.Sprint(lookFrom)}, "to": {fmt.Sprint(lookTo)}, "exclude_tag": {"private-document"}, "limit": {"100"}}
	if err := storeclient.Get(photoStoreURL+"/photos?"+query.Encode(), &photos); err != nil {
		return made, err
	}
	for _, picture := range photos {
		ok, err := link(ledgerURL, entry.ID, map[string]any{"ref": picture.ID, "bears_on": timeledgerstore.BearsOnContext,
			"reason": "a photo taken " + window}, "photos")
		if err != nil {
			return made, err
		}
		if ok {
			made++
		}
	}
	return made, nil
}

// link adds one link and reports whether it is new. A conflict (the operator
// took it off, or it bears on the entry some other way) is logged and left.
func link(ledgerURL, entryID string, body map[string]any, linkType string) (bool, error) {
	body["by"], body["by_kind"] = signer, timeledgerstore.ActorProgram
	var made timeledgerstore.Link
	err := storeclient.Do("POST", ledgerURL+"/time-entries/"+entryID+"/links/"+linkType, body, &made)
	if storeclient.IsStatus(err, 409) {
		log.Printf("did not link %s %v to %s: %v", linkType, body["ref"], entryID, err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return made.LinkedBy == signer && time.Since(time.Unix(made.LinkedAt, 0)) < time.Minute, nil
}
