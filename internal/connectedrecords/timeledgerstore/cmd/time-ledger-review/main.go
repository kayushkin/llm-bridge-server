// Command time-ledger-review asks a model, for each time entry that has ended
// and has evidence no review has seen, whether the operator did it. The model
// reads the entry with every record linked to it, read whole from the store
// that owns it, and the entries that could hold it, and answers an outcome
// with its reasons, the kinds the entry is, and the entry it belongs inside.
//
// The answer is kept as a review signed with the model's id. Kinds and the
// parent it names are applied as the model's, which never undoes the
// operator's. The model comes from model-store's role -role (claude-opus-5-5,
// chosen by the operator), asked through llm-bridge-server's POST /oneshot.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	timeledgerstore "github.com/kayushkin/time-ledger-store"
	"github.com/kayushkin/time-ledger-store/internal/storeclient"
)

// recordURLs says where each kind of linked record is read whole: the owning
// store's route for one id.
type recordURLs map[string]string

type reviewer struct {
	ledgerURL, bridgeURL, serviceToken, modelStoreURL, role string
	records                                                 recordURLs
	maxLinksPerType                                         int
	dryRun                                                  bool
	kinds                                                   map[string]string
	prices                                                  map[string][2]float64
	httpClient                                              *http.Client
}

func main() {
	settings, err := timeledgerstore.NewReviewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	review := reviewer{bridgeURL: settings.String(timeledgerstore.SettingBridgeURL), serviceToken: settings.String(timeledgerstore.SettingBridgeServiceToken),
		modelStoreURL: settings.String(timeledgerstore.SettingModelStoreURL), httpClient: &http.Client{Timeout: 5 * time.Minute}}
	flag.StringVar(&review.ledgerURL, "time-ledger-store-url", "http://127.0.0.1:8344", "where time-ledger-store answers")
	flag.StringVar(&review.role, "role", "time-entry-reviewer", "the model-store role whose model reviews")
	flag.IntVar(&review.maxLinksPerType, "max-links-per-type", 40, "the most records of one link type shown for one entry; the model is told how many more there are")
	maxEntries := flag.Int("max-entries", 50, "the most entries reviewed in one run")
	days := flag.Int("days", 60, "review entries that ended in the last this many days")
	onlyEntry := flag.String("entry", "", "review this one entry, whether or not it needs it")
	flag.BoolVar(&review.dryRun, "dry-run", false, "print the prompt for the first entry and stop")
	calendarStore := flag.String("calendar-store-url", "http://127.0.0.1:8342", "where calendar-store answers")
	locationStore := flag.String("location-store-url", "http://127.0.0.1:8333", "where location-store answers")
	photoStore := flag.String("photo-store-url", "http://127.0.0.1:8329", "where photo-store answers")
	discordStore := flag.String("discord-signup-store-url", "http://127.0.0.1:8312", "where discord-signup-store answers")
	eventStore := flag.String("event-store-url", "http://127.0.0.1:8308", "where event-store answers")
	messageStore := flag.String("message-store-url", "http://127.0.0.1:8337", "where message-store answers")
	budgetStore := flag.String("budget-store-url", "http://127.0.0.1:8340", "where budget-store answers")
	activityStore := flag.String("activity-store-url", "http://127.0.0.1:8336", "where activity-store answers")
	flag.Parse()
	review.records = recordURLs{
		"calendar_events":     *calendarStore + "/events/",
		"discord_events":      *discordStore + "/api/discord-events/",
		"event_store_events":  *eventStore + "/events/",
		"chat_messages":       *messageStore + "/messages/",
		"trips":               *locationStore + "/trips/",
		"visits":              *locationStore + "/visits/",
		"photos":              *photoStore + "/photos/",
		"transactions":        *budgetStore + "/transactions/",
		"discord_attendances": *discordStore + "/api/discord-attendance/",
		"activities":          *activityStore + "/activities/",
	}
	for _, name := range timeledgerstore.LinkTypeNames() {
		if review.records[name] == "" {
			log.Fatalf("no route reads a %s record", name)
		}
	}
	if err := review.readKinds(); err != nil {
		log.Fatal(err)
	}

	entries := []timeledgerstore.TimeEntry{}
	if *onlyEntry != "" {
		entries = append(entries, timeledgerstore.TimeEntry{ID: *onlyEntry})
	} else {
		query := url.Values{"needs_review": {"true"}, "from": {fmt.Sprint(time.Now().AddDate(0, 0, -*days).Unix())},
			"ended_before": {fmt.Sprint(time.Now().Unix())}, "limit": {fmt.Sprint(*maxEntries)}}
		if err := storeclient.Get(review.ledgerURL+"/time-entries?"+query.Encode(), &entries); err != nil {
			log.Fatal(err)
		}
	}
	totalCost, counts := 0.0, map[string]int{}
	for _, listed := range entries {
		outcome, cost, err := review.reviewEntry(listed.ID)
		if err != nil {
			log.Fatalf("%s: %v", listed.ID, err)
		}
		if review.dryRun {
			return
		}
		totalCost += cost
		counts[outcome]++
	}
	log.Printf("reviewed %d entries: %d done, %d skipped, %d unknown; $%.2f at list price", len(entries), counts["done"], counts["skipped"], counts["unknown"], totalCost)
}

func (review *reviewer) readKinds() error {
	var kinds []timeledgerstore.Kind
	if err := storeclient.Get(review.ledgerURL+"/kinds", &kinds); err != nil {
		return err
	}
	review.kinds = map[string]string{}
	for _, kind := range kinds {
		review.kinds[kind.Name] = kind.ID
	}
	return nil
}

const systemPrompt = `You keep the operator's time ledger: an account of what the operator actually did, when and where. Each entry is one thing on one date, brought in from a calendar, a Discord event, an event suggestion, or the operator's location history. Your job is to judge one entry: did the operator do it?

Answer "done" when the evidence shows the operator did it: they were at the place at the time, photos show it, a Discord attendance record says so, a charge was made there, or it is a journey from location history (journeys are measured, so they happened). Answer "skipped" when the evidence shows they did not: they were somewhere else for the whole time, or a message says it was called off. Answer "unknown" when the evidence does not settle it either way, including when there is no location data for the time at all. Never guess: a routine on the calendar is not evidence it happened, and a plausible place name is not a match unless the place or the time fits.

Online things (telehealth, video calls) happen wherever the operator is, so the place does not decide them. Times are given in the entry's own time zone.

Also say which kinds the entry is, from the kinds listed (an entry can be several), and which of the candidate parent entries it belongs inside, if any: a parent is a longer thing this one was part of, such as the journey it happened on. Give "" for no parent.

Write the reason in plain English, in two to four sentences, naming the records it rests on by their ids.`

var answerSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["outcome","reason","kind_names","parent_time_entry_id"],"properties":{
"outcome":{"type":"string","enum":["done","skipped","unknown"]},
"reason":{"type":"string"},
"kind_names":{"type":"array","items":{"type":"string"}},
"parent_time_entry_id":{"type":"string"}}}`)

type answer struct {
	Outcome           string   `json:"outcome"`
	Reason            string   `json:"reason"`
	KindNames         []string `json:"kind_names"`
	ParentTimeEntryID string   `json:"parent_time_entry_id"`
}

func localTime(unix int64, zoneName string) string {
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		return time.Unix(unix, 0).UTC().Format("Mon 2006-01-02 15:04 MST")
	}
	return time.Unix(unix, 0).In(zone).Format("Mon 2006-01-02 15:04 MST")
}

// readRecord reads one linked record whole from its owning store, leaving
// out fields that are bulk rather than meaning: raw source text and paths.
func (review *reviewer) readRecord(linkType, ref string) (map[string]any, error) {
	var record map[string]any
	if err := storeclient.Get(review.records[linkType]+url.PathEscape(ref), &record); err != nil {
		if storeclient.IsStatus(err, 404) {
			return map[string]any{"missing": "the owning store has no such record now"}, nil
		}
		return nil, err
	}
	for _, bulk := range []string{"raw", "sha256", "source_path", "source_machine", "embedding"} {
		delete(record, bulk)
	}
	return record, nil
}

func (review *reviewer) reviewEntry(entryID string) (string, float64, error) {
	var entry timeledgerstore.TimeEntry
	if err := storeclient.Get(review.ledgerURL+"/time-entries/"+entryID, &entry); err != nil {
		return "", 0, err
	}
	shownPerType, linksShown, omitted := map[string]int{}, []map[string]any{}, map[string]int{}
	for _, link := range entry.Links {
		if link.RemovedAt != nil {
			continue
		}
		if shownPerType[link.LinkType] >= review.maxLinksPerType {
			omitted[link.LinkType]++
			continue
		}
		shownPerType[link.LinkType]++
		record, err := review.readRecord(link.LinkType, link.Ref)
		if err != nil {
			return "", 0, err
		}
		for _, field := range []string{"starts_at", "ends_at", "started_at", "ended_at", "taken_at", "start_ts", "end_ts", "occurrence_starts_at"} {
			if value, ok := record[field].(float64); ok && value > 0 {
				record[field+"_local"] = localTime(int64(value), entry.TimeZone)
			}
		}
		shown := map[string]any{"link_type": link.LinkType, "ref": link.Ref, "bears_on": link.BearsOn, "linked_by": link.LinkedBy, "why_linked": link.Reason, "record": record}
		if link.OccurrenceStartsAt != nil {
			shown["date_of_series"] = localTime(*link.OccurrenceStartsAt, entry.TimeZone)
		}
		if link.OverlapSeconds != nil {
			shown["overlap_minutes"] = *link.OverlapSeconds / 60
		}
		linksShown = append(linksShown, shown)
	}
	var candidates []timeledgerstore.TimeEntry
	query := url.Values{"from": {fmt.Sprint(entry.StartsAt)}, "to": {fmt.Sprint(entry.EndsAt + 1)}, "limit": {"1000"}}
	if err := storeclient.Get(review.ledgerURL+"/time-entries?"+query.Encode(), &candidates); err != nil {
		return "", 0, err
	}
	parents := []map[string]any{}
	for _, candidate := range candidates {
		if candidate.ID != entry.ID && candidate.StartsAt <= entry.StartsAt && candidate.EndsAt >= entry.EndsAt &&
			candidate.EndsAt-candidate.StartsAt > entry.EndsAt-entry.StartsAt {
			parents = append(parents, map[string]any{"id": candidate.ID, "title": candidate.Title, "starts": localTime(candidate.StartsAt, entry.TimeZone),
				"ends": localTime(candidate.EndsAt, entry.TimeZone), "kinds": kindNames(candidate)})
		}
	}
	kindList := []string{}
	for name := range review.kinds {
		kindList = append(kindList, name)
	}
	payload := map[string]any{
		"entry": map[string]any{"id": entry.ID, "title": entry.Title, "starts": localTime(entry.StartsAt, entry.TimeZone), "ends": localTime(entry.EndsAt, entry.TimeZone),
			"all_day": entry.AllDay, "time_zone": entry.TimeZone, "place": entry.Place, "kinds": kindNames(entry), "parent_time_entry_id": entry.ParentTimeEntryID,
			"why_it_is_in_the_ledger": entry.CreatedReason},
		"linked_records":    linksShown,
		"records_not_shown": omitted,
		"candidate_parents": parents,
		"kinds_to_choose":   kindList,
		"now":               localTime(time.Now().Unix(), entry.TimeZone),
	}
	prompt, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		return "", 0, err
	}
	if review.dryRun {
		fmt.Println(string(prompt))
		return "", 0, nil
	}
	reply, err := review.ask(string(prompt))
	if err != nil {
		return "", 0, err
	}
	var parsed answer
	if err := json.Unmarshal(reply.Parsed, &parsed); err != nil {
		return "", 0, fmt.Errorf("%s answered %s: %w", reply.Model, reply.Parsed, err)
	}
	cost, err := review.cost(reply)
	if err != nil {
		return "", 0, err
	}
	linksSeen := []map[string]any{}
	for _, shown := range linksShown {
		linksSeen = append(linksSeen, map[string]any{"link_type": shown["link_type"], "ref": shown["ref"], "bears_on": shown["bears_on"]})
	}
	seen, err := json.Marshal(linksSeen)
	if err != nil {
		return "", 0, err
	}
	signer := "time-ledger-review (" + reply.Model + ")"
	input := timeledgerstore.ReviewInput{Outcome: parsed.Outcome, Reason: parsed.Reason, By: signer, ByKind: timeledgerstore.ActorModel, Model: reply.Model,
		InputTokens: int64(reply.Usage.InputTokens + reply.Usage.CacheReadTokens + reply.Usage.CacheWriteTokens), OutputTokens: int64(reply.Usage.OutputTokens),
		CostUSD: cost, LinksSeen: seen, SuggestedParentTimeEntryID: parsed.ParentTimeEntryID, SuggestedKindNames: parsed.KindNames}
	if err := storeclient.Do("POST", review.ledgerURL+"/time-entries/"+entry.ID+"/reviews", input, nil); err != nil {
		return "", 0, err
	}
	for _, name := range parsed.KindNames {
		kindID := review.kinds[name]
		if kindID == "" {
			log.Printf("%s: the model named kind %q, which does not exist; left out", entry.ID, name)
			continue
		}
		err := storeclient.Do("PUT", review.ledgerURL+"/time-entries/"+entry.ID+"/kinds/"+kindID,
			map[string]string{"by": signer, "by_kind": timeledgerstore.ActorModel, "reason": "the reviewer judged it " + name}, nil)
		if storeclient.IsStatus(err, 409) {
			log.Printf("%s: kind %s left off: %v", entry.ID, name, err)
		} else if err != nil {
			return "", 0, err
		}
	}
	if parsed.ParentTimeEntryID != "" && parsed.ParentTimeEntryID != entry.ParentTimeEntryID && entry.ParentSetByKind != timeledgerstore.ActorOperator {
		change := timeledgerstore.EntryChange{ParentTimeEntryID: &parsed.ParentTimeEntryID, By: signer, ByKind: timeledgerstore.ActorModel, Reason: "the reviewer placed it"}
		err := storeclient.Do("PATCH", review.ledgerURL+"/time-entries/"+entry.ID, change, nil)
		if storeclient.IsStatus(err, 409) || storeclient.IsStatus(err, 400) {
			log.Printf("%s: parent %q left as it was: %v", entry.ID, parsed.ParentTimeEntryID, err)
		} else if err != nil {
			return "", 0, err
		}
	}
	log.Printf("%s %q %s: %s ($%.3f)", entry.ID, entry.Title, localTime(entry.StartsAt, entry.TimeZone), parsed.Outcome, cost)
	return parsed.Outcome, cost, nil
}

func kindNames(entry timeledgerstore.TimeEntry) []string {
	names := []string{}
	for _, kind := range entry.Kinds {
		names = append(names, kind.Name)
	}
	return names
}

func (review *reviewer) ask(prompt string) (msg.OneShotResponse, error) {
	body, err := json.Marshal(msg.OneShotRequest{Prompt: prompt, SystemPrompt: systemPrompt, ModelRole: review.role, Caller: timeledgerstore.ReviewName,
		Schema: answerSchema, MaxTokens: 2000})
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, review.bridgeURL+"/oneshot", bytes.NewReader(body))
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-LLM-Bridge-Service-Token", review.serviceToken)
	response, err := review.httpClient.Do(request)
	if err != nil {
		return msg.OneShotResponse{}, fmt.Errorf("POST /oneshot (role %s): %w", review.role, err)
	}
	defer response.Body.Close()
	answerBody, err := io.ReadAll(response.Body)
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		return msg.OneShotResponse{}, fmt.Errorf("POST /oneshot (role %s): %d %s", review.role, response.StatusCode, bytes.TrimSpace(answerBody))
	}
	var reply msg.OneShotResponse
	if err := json.Unmarshal(answerBody, &reply); err != nil {
		return msg.OneShotResponse{}, fmt.Errorf("POST /oneshot answered something that is not a OneShotResponse: %w", err)
	}
	if len(reply.Parsed) == 0 {
		return msg.OneShotResponse{}, fmt.Errorf("%s (role %s) answered without the structured output: %q", reply.Model, review.role, strings.TrimSpace(reply.Text))
	}
	return reply, nil
}

// cost prices a reply at model-store's list prices, dollars per million tokens.
func (review *reviewer) cost(reply msg.OneShotResponse) (float64, error) {
	if review.prices == nil {
		var models []struct {
			ID         string  `json:"id"`
			InputCost  float64 `json:"input_cost"`
			OutputCost float64 `json:"output_cost"`
		}
		if err := storeclient.Get(review.modelStoreURL+"/api/models", &models); err != nil {
			return 0, fmt.Errorf("read model-store prices: %w", err)
		}
		review.prices = map[string][2]float64{}
		for _, model := range models {
			review.prices[model.ID] = [2]float64{model.InputCost, model.OutputCost}
		}
	}
	price, known := review.prices[reply.Model]
	if !known {
		return 0, fmt.Errorf("model-store has no price for %s", reply.Model)
	}
	input := float64(reply.Usage.InputTokens + reply.Usage.CacheReadTokens + reply.Usage.CacheWriteTokens)
	return (input*price[0] + float64(reply.Usage.OutputTokens)*price[1]) / 1e6, nil
}
