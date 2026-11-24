// Command journal-timeline-discord-events puts the Discord events the
// operator went to on journal timelines: each on the timeline kept for its
// server, and each again on the timeline of every other person who went and
// has one.
//
// It works from discord-signup-store's attendance records, never from the
// roster: a roster says who signed up, an attendance record says who went and
// what that rests on. people-store says which person a Discord account
// belongs to. Items cite the event (discord_event_000126) and the attendance
// record they rest on (discord_attendance_000001).
//
// Nothing is written twice. A timeline that already has an item citing the
// event for that date, deleted ones included, gets no other: an item written
// by hand wins, and one the operator removed stays removed. A hand-written
// item on the server's timeline gets the operator's attendance record added
// to its sources, so it can be cited too.
//
// The command owns the items it writes — those whose body starts with one of
// ownedBodyPrefixes — and keeps them true: it rewrites one whose text no
// longer matches the attendance, and deletes one for a date the operator has
// no attendance record for.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	journalstore "github.com/kayushkin/journal-store"
)

var client = &http.Client{Timeout: time.Minute}

// sameDateWindow is how close an item's time must be to an attendance
// record's date to count as that date: a repeating event's dates are a week
// or more apart.
const sameDateWindow = 36 * 3600

// ownedBodyPrefixes start the bodies of items this command writes, now and
// before it worked from attendance.
var ownedBodyPrefixes = []string{"Went to [", "Both went to [", "Signed up for [", "Both signed up for [", withdrawnBodyPrefix}

// withdrawnBodyPrefix starts the body an owned item is given just before this
// command deletes it. A deleted item with this body was removed by the
// command, so it is restored if an attendance record comes for its date; a
// deleted item with any other body was removed by a person and stays removed.
const withdrawnBodyPrefix = "No attendance record for this date: "

type attendance struct {
	Ref                string `json:"ref"`
	EventID            int64  `json:"event_id"`
	EventRef           string `json:"event_ref"`
	OccurrenceStartsAt int64  `json:"occurrence_starts_at"`
	DiscordUserID      string `json:"discord_user_id"`
	DisplayName        string `json:"display_name"`
	Basis              string `json:"basis"`
}

type discordEvent struct {
	ID       int64  `json:"id"`
	Ref      string `json:"ref"`
	GuildID  string `json:"guild_id"`
	Name     string `json:"name"`
	StartsAt int64  `json:"starts_at"`
	EndsAt   int64  `json:"ends_at"`
	Location string `json:"location"`
}

type timelineItem struct {
	ID         string   `json:"id"`
	OccurredAt int64    `json:"occurred_at"`
	Body       string   `json:"body"`
	Sources    []string `json:"sources"`
	DeletedAt  int64    `json:"deleted_at"`
}

type person struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func request(method, address string, payload any, wantStatus int, into any) (found bool, err error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(encoded)
	}
	httpRequest, err := http.NewRequest(method, address, body)
	if err != nil {
		return false, err
	}
	if payload != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return false, nil
	}
	if response.StatusCode != wantStatus {
		return false, fmt.Errorf("%s %s: %s: %s", method, address, response.Status, strings.TrimSpace(string(answer)))
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			return false, fmt.Errorf("%s %s: %w", method, address, err)
		}
	}
	return true, nil
}

func mustGet(address string, into any) error {
	found, err := request(http.MethodGet, address, nil, http.StatusOK, into)
	if err == nil && !found {
		err = fmt.Errorf("GET %s: 404", address)
	}
	return err
}

func markdownText(text string) string {
	return strings.NewReplacer("[", "(", "]", ")").Replace(text)
}

func owned(item timelineItem) bool {
	for _, prefix := range ownedBodyPrefixes {
		if strings.HasPrefix(item.Body, prefix) {
			return true
		}
	}
	return false
}

func cites(item timelineItem, ref string) bool {
	for _, source := range item.Sources {
		if source == ref {
			return true
		}
	}
	return false
}

func sameDate(item timelineItem, startsAt int64) bool {
	distance := item.OccurredAt - startsAt
	if distance < 0 {
		distance = -distance
	}
	return distance <= sameDateWindow
}

// date is one date of one event the operator went to, with everyone who went.
type date struct {
	Event    discordEvent
	StartsAt int64
	Operator attendance
	Others   []attendance
}

// wanted is an item this command should have on a timeline.
type wanted struct {
	TimelineID string
	Date       *date
	Item       journalstore.TimelineItemInput
}

type importer struct {
	journalURL, signupURL, peopleURL string
	operatorDiscordID                string
	matrixHomeserver                 string
	guildTimelines                   map[string]string
	dryRun                           bool

	personByDiscord                           map[string]*person
	personTimelines                           map[string][]string
	items                                     map[string][]timelineItem
	added, changed, removed, restored, linked int
}

func (importer *importer) personOf(discordUserID string) (*person, error) {
	if found, ok := importer.personByDiscord[discordUserID]; ok {
		return found, nil
	}
	handles := [][2]string{{"discord", discordUserID}}
	if importer.matrixHomeserver != "" {
		handles = append(handles, [2]string{"matrix_user", "@discord_" + discordUserID + ":" + importer.matrixHomeserver})
	}
	var found *person
	for _, handle := range handles {
		var answer struct {
			Person person `json:"person"`
		}
		ok, err := request(http.MethodGet, importer.peopleURL+"/identities/resolve?kind="+handle[0]+"&value="+url.QueryEscape(handle[1]), nil, http.StatusOK, &answer)
		if err != nil {
			return nil, err
		}
		if ok {
			found = &answer.Person
			break
		}
	}
	importer.personByDiscord[discordUserID] = found
	return found, nil
}

func (importer *importer) timelinesAbout(personID string) ([]string, error) {
	if ids, ok := importer.personTimelines[personID]; ok {
		return ids, nil
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if err := mustGet(importer.journalURL+"/entries?kind=timeline&about_ref="+url.QueryEscape(personID), &entries); err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	importer.personTimelines[personID] = ids
	return ids, nil
}

func (importer *importer) itemsOf(timelineID string) ([]timelineItem, error) {
	if items, ok := importer.items[timelineID]; ok {
		return items, nil
	}
	var items []timelineItem
	if err := mustGet(importer.journalURL+"/timelines/"+timelineID+"/items?include_deleted=true", &items); err != nil {
		return nil, err
	}
	importer.items[timelineID] = items
	return items, nil
}

func (importer *importer) nameLink(row attendance) (string, *person, error) {
	found, err := importer.personOf(row.DiscordUserID)
	if err != nil {
		return "", nil, err
	}
	if found != nil {
		return fmt.Sprintf("[%s](ref:%s)", markdownText(found.DisplayName), found.ID), found, nil
	}
	return markdownText(row.DisplayName), nil, nil
}

// plan works out every item this command should have, for every date the
// operator went to.
func (importer *importer) plan(dates []*date) ([]wanted, error) {
	var plans []wanted
	for _, one := range dates {
		groupTimelineID := importer.guildTimelines[one.Event.GuildID]
		where := ""
		if one.Event.Location != "" {
			where = " at " + markdownText(strings.Join(strings.Fields(one.Event.Location), " "))
		}
		endsAt := int64(0)
		if one.StartsAt == one.Event.StartsAt && one.Event.EndsAt > one.Event.StartsAt {
			endsAt = one.Event.EndsAt
		}
		var names []string
		rosterOnly := 0
		type otherPerson struct {
			row    attendance
			person *person
		}
		var people []otherPerson
		for _, other := range one.Others {
			name, found, err := importer.nameLink(other)
			if err != nil {
				return nil, err
			}
			names = append(names, name)
			if other.Basis == "roster" {
				rosterOnly++
			}
			if found != nil {
				people = append(people, otherPerson{other, found})
			}
		}
		withWhom := "No one else is recorded as going."
		if len(names) > 0 {
			withWhom = fmt.Sprintf("Also there (%d): %s.", len(names), strings.Join(names, ", "))
			if rosterOnly > 0 {
				withWhom += fmt.Sprintf(" Recorded from the roster alone (signed up, and nothing says they stayed away): %d of %d.", rosterOnly, len(names))
			}
		}
		body := fmt.Sprintf("Went to [%s](ref:%s)%s ([his attendance](ref:%s)). %s",
			markdownText(one.Event.Name), one.Event.Ref, where, one.Operator.Ref, withWhom)
		plans = append(plans, wanted{TimelineID: groupTimelineID, Date: one, Item: journalstore.TimelineItemInput{
			OccurredAt: one.StartsAt, EndsAt: endsAt, Precision: "minute", Title: one.Event.Name, Body: body,
			Sources: []string{one.Event.Ref, one.Operator.Ref},
		}})
		for _, other := range people {
			timelineIDs, err := importer.timelinesAbout(other.person.ID)
			if err != nil {
				return nil, err
			}
			for _, timelineID := range timelineIDs {
				body := fmt.Sprintf("Both went to [%s](ref:%s)%s ([their attendance](ref:%s), [his](ref:%s)), with %d others; [the group's timeline](ref:%s) lists them.",
					markdownText(one.Event.Name), one.Event.Ref, where, other.row.Ref, one.Operator.Ref, len(one.Others)-1, groupTimelineID)
				plans = append(plans, wanted{TimelineID: timelineID, Date: one, Item: journalstore.TimelineItemInput{
					OccurredAt: one.StartsAt, EndsAt: endsAt, Precision: "minute", Title: one.Event.Name, Body: body,
					Sources: []string{one.Event.Ref, other.row.Ref, one.Operator.Ref},
				}})
			}
		}
	}
	return plans, nil
}

func sameSources(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (importer *importer) apply(plans []wanted) error {
	// What each timeline should hold, by event ref and date.
	kept := map[string]bool{}
	for _, plan := range plans {
		items, err := importer.itemsOf(plan.TimelineID)
		if err != nil {
			return err
		}
		var match *timelineItem
		for index := range items {
			if cites(items[index], plan.Date.Event.Ref) && sameDate(items[index], plan.Date.StartsAt) {
				match = &items[index]
				break
			}
		}
		switch {
		case match == nil:
			importer.added++
			log.Printf("add to %s: %s %q", plan.TimelineID, plan.Date.Event.Ref, plan.Item.Title)
			if !importer.dryRun {
				if _, err := request(http.MethodPost, importer.journalURL+"/timelines/"+plan.TimelineID+"/items",
					journalstore.TimelineItemsRequest{Items: []journalstore.TimelineItemInput{plan.Item}}, http.StatusCreated, nil); err != nil {
					return err
				}
			}
		case match.DeletedAt != 0 && strings.HasPrefix(match.Body, withdrawnBodyPrefix):
			kept[match.ID] = true
			importer.restored++
			log.Printf("restore %s on %s: an attendance record came for it", match.ID, plan.TimelineID)
			if !importer.dryRun {
				if _, err := request(http.MethodPost, importer.journalURL+"/timeline-items/"+match.ID+"/restore", nil, http.StatusOK, nil); err != nil {
					return err
				}
				if _, err := request(http.MethodPatch, importer.journalURL+"/timeline-items/"+match.ID,
					map[string]any{"body": plan.Item.Body, "sources": plan.Item.Sources, "occurred_at": plan.Item.OccurredAt,
						"ends_at": plan.Item.EndsAt, "title": plan.Item.Title}, http.StatusOK, nil); err != nil {
					return err
				}
			}
		case match.DeletedAt != 0:
			kept[match.ID] = true
		case owned(*match):
			kept[match.ID] = true
			if match.Body == plan.Item.Body && sameSources(match.Sources, plan.Item.Sources) {
				continue
			}
			importer.changed++
			log.Printf("rewrite %s on %s: %q", match.ID, plan.TimelineID, plan.Item.Title)
			if !importer.dryRun {
				if _, err := request(http.MethodPatch, importer.journalURL+"/timeline-items/"+match.ID,
					map[string]any{"body": plan.Item.Body, "sources": plan.Item.Sources, "occurred_at": plan.Item.OccurredAt,
						"ends_at": plan.Item.EndsAt, "title": plan.Item.Title}, http.StatusOK, nil); err != nil {
					return err
				}
			}
		default:
			// Written by hand: leave the text alone, but let it cite the
			// attendance record the plan rests on.
			kept[match.ID] = true
			var missing []string
			for _, source := range plan.Item.Sources[1:] {
				if !cites(*match, source) {
					missing = append(missing, source)
				}
			}
			if len(missing) == 0 {
				continue
			}
			importer.linked++
			log.Printf("cite %v on hand-written %s", missing, match.ID)
			if !importer.dryRun {
				if _, err := request(http.MethodPatch, importer.journalURL+"/timeline-items/"+match.ID,
					map[string]any{"sources": append(append([]string{}, match.Sources...), missing...)}, http.StatusOK, nil); err != nil {
					return err
				}
			}
		}
	}
	// Owned items no plan wants: a date he has no attendance record for.
	timelineIDs := make([]string, 0, len(importer.items))
	for timelineID := range importer.items {
		timelineIDs = append(timelineIDs, timelineID)
	}
	for _, timelineID := range importer.guildTimelines {
		if _, ok := importer.items[timelineID]; !ok {
			if _, err := importer.itemsOf(timelineID); err != nil {
				return err
			}
			timelineIDs = append(timelineIDs, timelineID)
		}
	}
	sort.Strings(timelineIDs)
	for _, timelineID := range timelineIDs {
		for _, item := range importer.items[timelineID] {
			if item.DeletedAt != 0 || kept[item.ID] || !owned(item) {
				continue
			}
			importer.removed++
			log.Printf("delete %s on %s: he has no attendance record for it", item.ID, timelineID)
			if !importer.dryRun {
				withdrawn := withdrawnBodyPrefix + strings.TrimPrefix(item.Body, withdrawnBodyPrefix)
				if _, err := request(http.MethodPatch, importer.journalURL+"/timeline-items/"+item.ID,
					map[string]any{"body": withdrawn}, http.StatusOK, nil); err != nil {
					return err
				}
				if _, err := request(http.MethodDelete, importer.journalURL+"/timeline-items/"+item.ID, nil, http.StatusNoContent, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ownedTimelines are the person timelines that hold an owned item, so that a
// stale one there is found even when no plan reaches that timeline now.
func (importer *importer) loadPersonTimelinesWithOwnedItems() error {
	var timelines []struct {
		ID       string `json:"id"`
		AboutRef string `json:"about_ref"`
	}
	if err := mustGet(importer.journalURL+"/entries?kind=timeline&limit=1000", &timelines); err != nil {
		return err
	}
	for _, timeline := range timelines {
		if timeline.AboutRef == "" {
			continue
		}
		if _, err := importer.itemsOf(timeline.ID); err != nil {
			return err
		}
	}
	return nil
}

func (importer *importer) run() error {
	var mine struct {
		Attendance []attendance `json:"attendance"`
	}
	if err := mustGet(importer.signupURL+"/api/attendance?discord_user_id="+url.QueryEscape(importer.operatorDiscordID), &mine); err != nil {
		return err
	}
	var dates []*date
	events := map[int64]discordEvent{}
	for _, row := range mine.Attendance {
		event, ok := events[row.EventID]
		if !ok {
			if err := mustGet(importer.signupURL+"/api/discord-events/"+row.EventRef, &event); err != nil {
				return err
			}
			events[row.EventID] = event
		}
		if _, listed := importer.guildTimelines[event.GuildID]; !listed {
			continue
		}
		var all struct {
			Attendance []attendance `json:"attendance"`
		}
		if err := mustGet(fmt.Sprintf("%s/api/attendance?event_id=%d", importer.signupURL, row.EventID), &all); err != nil {
			return err
		}
		one := &date{Event: event, StartsAt: row.OccurrenceStartsAt, Operator: row}
		for _, other := range all.Attendance {
			if other.OccurrenceStartsAt == row.OccurrenceStartsAt && other.DiscordUserID != importer.operatorDiscordID {
				one.Others = append(one.Others, other)
			}
		}
		dates = append(dates, one)
	}
	if err := importer.loadPersonTimelinesWithOwnedItems(); err != nil {
		return err
	}
	plans, err := importer.plan(dates)
	if err != nil {
		return err
	}
	if err := importer.apply(plans); err != nil {
		return err
	}
	verb := ""
	if importer.dryRun {
		verb = " (dry run: nothing written)"
	}
	log.Printf("%d dates he went to; added %d, restored %d, rewrote %d, deleted %d, added his record to %d hand-written items%s",
		len(dates), importer.added, importer.restored, importer.changed, importer.removed, importer.linked, verb)
	return nil
}

type guildTimelines map[string]string

func (g guildTimelines) String() string { return fmt.Sprint(map[string]string(g)) }
func (g guildTimelines) Set(value string) error {
	guildID, timelineID, ok := strings.Cut(value, "=")
	if !ok || guildID == "" || timelineID == "" {
		return fmt.Errorf("want <guild id>=<timeline entry id>, got %q", value)
	}
	g[guildID] = timelineID
	return nil
}

func main() {
	journalURL := flag.String("journal-store-url", "http://"+journalstore.DefaultListenAddress, "journal-store's base URL")
	signupURL := flag.String("discord-signup-store-url", "", "discord-signup-store's base URL (required)")
	peopleURL := flag.String("people-store-url", "", "people-store's base URL (required)")
	operatorDiscordID := flag.String("operator-discord-id", "", "the Discord user id whose events these are (required)")
	matrixHomeserver := flag.String("matrix-homeserver", "", "also find a person by the Matrix handle @discord_<id>:<this> that multichat's bridge gives a Discord account; empty: only by kind discord")
	guilds := guildTimelines{}
	flag.Var(guilds, "guild-timeline", "<guild id>=<timeline entry id>: the timeline for that server's events; repeat for each server. Events in other servers are left alone")
	dryRun := flag.Bool("dry-run", false, "say what would change and write nothing")
	flag.Parse()
	if *signupURL == "" || *peopleURL == "" || *operatorDiscordID == "" || len(guilds) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	importer := &importer{journalURL: *journalURL, signupURL: *signupURL, peopleURL: *peopleURL,
		operatorDiscordID: *operatorDiscordID, matrixHomeserver: *matrixHomeserver, guildTimelines: guilds, dryRun: *dryRun,
		personByDiscord: map[string]*person{}, personTimelines: map[string][]string{}, items: map[string][]timelineItem{}}
	if err := importer.run(); err != nil {
		log.Fatal(err)
	}
}
