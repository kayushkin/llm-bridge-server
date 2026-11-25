// Command message-import-multichat imports multichat's message log into
// message-store, one row per message.
//
//	message-import-multichat [-window 72h] [-self-person person_000001] \
//	    [-room '!room:example.org']… [-discord-server <server id>]… [-dry-run]
//
// Only direct messages and groups of at most eight members, the user
// included, are imported, as GET /conversations counts them; a Discord
// server's channels only when -discord-server names the server, and any
// other room only when -room names it. A room multichat no longer lists is
// left out unless -room names it, since its size is unknown.
//
// A row is filed by its Discord message id when it has one, so it is the
// same row the Discord data package writes; every other row by its multichat
// ref. multichat gives the id to rows it imported from a Discord history
// fetch and to live Discord rows; a live Discord message that is text and
// attachments is several rows sharing the id, joined here into one message.
// A live row stored under its multichat ref before it carried the id is
// named in supersedes, and message-store retires it. Every Discord
// conversation is its channel; every other conversation its Matrix room. An
// edit rewrites the message's text and marks it edited; a delete marks it
// deleted on the platform and keeps the text. Pictures are cited by the
// photo-store ids photo-store filed under the row's ref.
//
// -window reads only the messages sent in that span before now, plus every
// deleted message, every message an edit in the span changed, and every
// message a read message replies to; message-store skips the rows that did
// not change. Without -window the whole log is read.
//
// MULTICHAT_TOKEN is the bearer token multichat asks for; it is never
// printed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	messagestore "github.com/kayushkin/message-store"
	"github.com/kayushkin/message-store/internal/importclient"
	"github.com/kayushkin/message-store/internal/photolookup"
)

const importerName = "message-import-multichat"

// pageSize is GET /message-log's largest page; a test pages smaller.
var pageSize = 1000

type repeated []string

func (values *repeated) String() string { return strings.Join(*values, ",") }

func (values *repeated) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("an empty value")
	}
	*values = append(*values, value)
	return nil
}

func main() {
	var rooms, servers repeated
	flag.Var(&rooms, "room", "a Matrix room id to import whatever its size (repeat)")
	flag.Var(&servers, "discord-server", "a Discord server id whose bridged channels are imported (repeat)")
	window := flag.Duration("window", 0, "read only messages sent this long before now, plus deleted, edited and replied-to ones; 0 reads the whole log")
	selfPerson := flag.String("self-person", "", "the user's people-store id: a handle people-store gives to this person is the user's")
	dryRun := flag.Bool("dry-run", false, "read everything and count what would be sent; send nothing")
	flag.Parse()
	if *window < 0 || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	settings, err := messagestore.NewImporterSettingsRegistry(importerName, servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: settings: %v\n", importerName, err)
		os.Exit(2)
	}
	token := settings.String(messagestore.SettingMultichatToken)
	if token == "" {
		fmt.Fprintf(os.Stderr, "%s: MULTICHAT_TOKEN is not set\n", importerName)
		os.Exit(2)
	}
	run := runner{
		multichat:      multichatClient{base: strings.TrimRight(settings.String(messagestore.SettingMultichatURL), "/") + "/api", token: token},
		storeURL:       settings.String(messagestore.SettingStoreURL),
		peopleStoreURL: strings.TrimRight(settings.String(messagestore.SettingPeopleStoreURL), "/"),
		photoStoreURL:  strings.TrimRight(settings.String(messagestore.SettingPhotoStoreURL), "/"),
		selection:      selection{rooms: set(rooms), discordServers: set(servers)},
		window:         *window, selfPerson: *selfPerson, dryRun: *dryRun,
	}
	if err := run.run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", importerName, err)
		os.Exit(1)
	}
}

func set(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// getJSON reads address into out; found is false on a 404.
func getJSON(address string, header http.Header, out any) (found bool, err error) {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return false, err
	}
	request.Header = header
	response, err := httpClient.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s answered %d: %s", strings.SplitN(address, "?", 2)[0], response.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, json.Unmarshal(body, out)
}

type multichatClient struct{ base, token string }

func (m multichatClient) get(path string, query url.Values, out any) error {
	address := m.base + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	found, err := getJSON(address, http.Header{"Authorization": {"Bearer " + m.token}}, out)
	if err == nil && !found {
		err = fmt.Errorf("GET %s: 404", path)
	}
	return err
}

// logReader pages GET /message-log newest first. multichat answers rows sent
// strictly before `before`, so a next page asked from the last row's moment
// would skip rows that share it. The next page is asked from that moment plus
// a nanosecond, dropping rows already read; a page that brings nothing new
// means every row at that moment is read, and the reader asks again from the
// moment itself.
type logReader struct {
	multichat multichatClient
	deleted   bool
	// platform, when set, reads only that multichat platform.
	platform string
	// before is the oldest moment read so far; zero before the first page.
	before time.Time
	seen   map[string]bool
	done   bool
}

// next is the following page's new rows and edits; empty at the end.
func (r *logReader) next() (logPage, error) {
	for nudge := time.Nanosecond; !r.done; nudge = 0 {
		query := url.Values{"limit": {fmt.Sprint(pageSize)}, "include_edits": {"true"}}
		if r.deleted {
			query.Set("deleted", "true")
		}
		if r.platform != "" {
			query.Set("platform", r.platform)
		}
		if !r.before.IsZero() {
			query.Set("before", r.before.Add(nudge).UTC().Format(time.RFC3339Nano))
		}
		var page logPage
		if err := r.multichat.get("/message-log", query, &page); err != nil {
			return logPage{}, err
		}
		if len(page.Messages) < pageSize {
			r.done = true
		}
		var fresh []loggedMessage
		for _, row := range page.Messages {
			if !r.seen[row.Ref] {
				r.seen[row.Ref] = true
				fresh = append(fresh, row)
			}
		}
		if len(page.Messages) > 0 {
			r.before = page.Messages[len(page.Messages)-1].SentAt
		}
		if len(fresh) > 0 {
			page.Messages = fresh
			return page, nil
		}
		if nudge == 0 && !r.done {
			return logPage{}, fmt.Errorf("GET /message-log answered %d rows sent before %s, all already read", len(page.Messages), r.before.Format(time.RFC3339Nano))
		}
	}
	return logPage{}, nil
}

type runner struct {
	multichat                     multichatClient
	storeURL                      string
	peopleStoreURL, photoStoreURL string
	selection                     selection
	window                        time.Duration
	selfPerson                    string
	dryRun                        bool
}

// gathered is what the reading pass collected.
type gathered struct {
	// imported are the rows to import, newest first.
	imported []loggedMessage
	skipped  map[string]int
	edits    map[string][]loggedMessage
	events   map[string]loggedMessage
	// unresolved counts imported rows whose reply or edit target is not in
	// the log.
	unresolved int
	read       int
}

func (run runner) run(out io.Writer) error {
	var listed []room
	if err := run.multichat.get("/conversations", nil, &listed); err != nil {
		return err
	}
	rooms := map[string]room{}
	for _, item := range listed {
		rooms[item.RoomID] = item
	}
	cutoff := time.Time{}
	if run.window > 0 {
		cutoff = time.Now().Add(-run.window)
	}
	read, err := gather(run.multichat, run.selection, rooms, cutoff)
	if err != nil {
		return err
	}
	reasons := make([]string, 0, len(read.skipped))
	for reason, count := range read.skipped {
		reasons = append(reasons, fmt.Sprintf("%d from %s", count, reason))
	}
	sort.Strings(reasons)
	fmt.Fprintf(out, "read %d log rows; importing %d messages; left out %s; %d replies or edits point at messages not in the log\n",
		read.read, len(read.imported), strings.Join(reasons, ", "), read.unresolved)

	people, err := run.resolvePeople(read.imported, rooms)
	if err != nil {
		return err
	}
	photos, err := run.photoIndex()
	if err != nil {
		return err
	}
	selfUserIDs, err := run.findSelfUserIDs(read.imported, rooms, people)
	if err != nil {
		return err
	}
	googleMessagesRows, err := readGoogleMessagesRows(run.multichat)
	if err != nil {
		return err
	}
	twins := matchGoogleMessagesTwins(googleMessagesRows)
	read.imported = twins.apply(read.imported, run.selection, rooms)
	fmt.Fprintf(out, "Google Messages: %d web rooms have a bridge room for the same conversation, %d do not, %d have a conversation id this command cannot read; %d web rows are the same message as a bridge row and are filed as it\n",
		twins.twinRooms, twins.webRoomsWithoutTwin, twins.unreadableWebRooms, len(twins.bridgeRowOfWebRow))
	c := converter{rooms: rooms, people: people, selfPerson: run.selfPerson, selfUserIDs: selfUserIDs, photos: photos, events: read.events, edits: read.edits,
		supersededByRow: twins.webRefsOfBridgeRow}
	conversations, err := c.conversations(read.imported)
	if err != nil {
		return err
	}
	selfHandles := 0
	for _, conversation := range conversations {
		for _, participant := range conversation.Participants {
			if participant.IsSelf {
				selfHandles++
			}
		}
	}
	fmt.Fprintf(out, "%d conversations; %d of their participant rows are the user's (%d accounts multichat marks as the user's); people-store names %d handles\n",
		len(conversations), selfHandles, len(selfUserIDs), len(people))

	client := importclient.New(run.storeURL, run.dryRun)
	description := "the whole log"
	if run.window > 0 {
		description = "messages sent in the last " + run.window.String() + ", with every deleted, edited and replied-to one"
	}
	if err := client.Begin(importerName, "multichat's message log: "+description); err != nil {
		return err
	}
	for _, conversation := range conversations {
		if err := client.AddConversation(conversation); err != nil {
			return err
		}
	}
	messages, err := c.messages(read.imported)
	if err != nil {
		return err
	}
	texts, err := readPhoneTexts(run.storeURL)
	if err != nil {
		return err
	}
	copies := matchPhoneCopies(messages, texts)
	messages, supersessions := copies.split(messages)
	fmt.Fprintf(out, "%d texts are copies of the %d phone texts message-store holds and are retired in favor of them\n", len(copies.phoneOf), len(texts))
	photoCount, merged := 0, 0
	for _, message := range messages {
		if len(message.Supersedes) > 1 {
			merged++
		}
		for _, attachment := range message.Attachments {
			if attachment.PhotoID != "" {
				photoCount++
			}
		}
		if err := client.AddMessage(message); err != nil {
			return err
		}
	}
	for _, supersession := range supersessions {
		if err := client.AddSupersession(supersession); err != nil {
			return err
		}
	}
	mergeInputs, err := run.readMergeInputs(rooms)
	if err != nil {
		return err
	}
	merges, mergeReport := planConversationMerges(mergeInputs)
	fmt.Fprintf(out, "Merges: %s\n", mergeReport)
	for _, merge := range merges {
		if err := client.AddConversationMerge(merge); err != nil {
			return err
		}
	}
	if err := client.Finish(); err != nil {
		return err
	}
	fmt.Fprintf(out, "%d messages from %d rows (%d Discord messages joined from several rows); %d photo-store pictures cited\n",
		len(messages), len(read.imported), merged, photoCount)
	fmt.Fprintln(out, client.Report())
	return nil
}

// gather reads the log: every deleted message, then newest first until the
// cutoff, and on past it until every imported row's edit and reply targets
// are found or the log ends. A zero cutoff reads the whole log.
func gather(multichat multichatClient, chosen selection, rooms map[string]room, cutoff time.Time) (gathered, error) {
	read := gathered{skipped: map[string]int{}, edits: map[string][]loggedMessage{}, events: map[string]loggedMessage{}}
	importedRefs := map[string]bool{}
	editRefs := map[string]bool{}
	// needed are event ids an imported row points at, not yet read.
	needed := map[string]bool{}
	// editedTargets are event ids an edit in an imported room changed.
	editedTargets := map[string]bool{}
	want := func(eventID string) {
		if _, known := read.events[eventID]; eventID != "" && !known {
			needed[eventID] = true
		}
	}
	addEdit := func(edit loggedMessage) {
		if editRefs[edit.Ref] {
			return
		}
		editRefs[edit.Ref] = true
		read.edits[edit.ReplacesEventID] = append(read.edits[edit.ReplacesEventID], edit)
	}
	importRow := func(row loggedMessage) {
		if importedRefs[row.Ref] {
			return
		}
		if reason := chosen.skipReason(row, rooms); reason != "" {
			read.skipped[reason]++
			return
		}
		importedRefs[row.Ref] = true
		read.imported = append(read.imported, row)
		want(row.ReplyToEventID)
	}
	take := func(page logPage, inWindow func(loggedMessage) bool) {
		for _, edits := range page.Edits {
			for _, edit := range edits {
				addEdit(edit)
			}
		}
		for _, row := range page.Messages {
			read.read++
			if row.ReplacesEventID != "" {
				addEdit(row)
				if inWindow(row) && chosen.skipReason(row, rooms) == "" {
					editedTargets[row.ReplacesEventID] = true
					want(row.ReplacesEventID)
				}
				continue
			}
			if row.EventID != "" {
				read.events[row.EventID] = row
				delete(needed, row.EventID)
			}
			if inWindow(row) || editedTargets[row.EventID] {
				importRow(row)
			}
		}
	}
	deleted := &logReader{multichat: multichat, deleted: true, seen: map[string]bool{}}
	for {
		page, err := deleted.next()
		if err != nil {
			return gathered{}, err
		}
		if len(page.Messages) == 0 {
			break
		}
		take(page, func(loggedMessage) bool { return true })
	}
	all := &logReader{multichat: multichat, seen: map[string]bool{}}
	for {
		page, err := all.next()
		if err != nil {
			return gathered{}, err
		}
		if len(page.Messages) == 0 {
			break
		}
		take(page, func(row loggedMessage) bool { return cutoff.IsZero() || !row.SentAt.Before(cutoff) })
		if !cutoff.IsZero() && all.before.Before(cutoff) && len(needed) == 0 {
			break
		}
	}
	read.unresolved = len(needed)
	// Rows come from two passes; keep the whole newest first.
	sort.SliceStable(read.imported, func(i, j int) bool { return read.imported[i].SentAt.After(read.imported[j].SentAt) })
	for target := range read.edits {
		sort.SliceStable(read.edits[target], func(i, j int) bool { return read.edits[target][i].SentAt.Before(read.edits[target][j].SentAt) })
	}
	return read, nil
}

// resolvePeople asks people-store whose each imported handle and listed
// member is, as a Matrix user and, on Discord, as a Discord account.
func (run runner) resolvePeople(rows []loggedMessage, rooms map[string]room) (map[string]string, error) {
	people := map[string]string{}
	asked := map[string]bool{}
	ask := func(matrixUserID string) error {
		if asked[matrixUserID] {
			return nil
		}
		asked[matrixUserID] = true
		lookups := []url.Values{{"kind": {"matrix_user"}, "value": {matrixUserID}}}
		if match := discordPuppet.FindStringSubmatch(matrixUserID); match != nil {
			lookups = append(lookups, url.Values{"kind": {"discord"}, "value": {match[1]}})
		}
		for _, lookup := range lookups {
			var resolution struct {
				Person struct {
					ID string `json:"id"`
				} `json:"person"`
			}
			found, err := getJSON(run.peopleStoreURL+"/identities/resolve?"+lookup.Encode(), nil, &resolution)
			if err != nil {
				return err
			}
			if found {
				if resolution.Person.ID == "" {
					return fmt.Errorf("people-store resolved a handle to a person with no id")
				}
				people[matrixUserID] = resolution.Person.ID
				return nil
			}
		}
		return nil
	}
	roomsSeen := map[string]bool{}
	for _, row := range rows {
		if err := ask(row.SenderUserID); err != nil {
			return nil, err
		}
		if roomsSeen[row.RoomID] {
			continue
		}
		roomsSeen[row.RoomID] = true
		for _, member := range rooms[row.RoomID].Members {
			if err := ask(member.UserID); err != nil {
				return nil, err
			}
		}
	}
	return people, nil
}

// findSelfUserIDs asks multichat which senders are the user's own accounts.
// GET /conversations lists everyone in a room but the user, so a room with a
// sender it does not list and people-store does not give to the user is read
// once through GET /conversations/{room}/messages, whose is_me says which
// senders are the user's. A Discord server channel lists only some members
// and is not asked.
func (run runner) findSelfUserIDs(rows []loggedMessage, rooms map[string]room, people map[string]string) (map[string]bool, error) {
	self := map[string]bool{}
	asked := map[string]bool{}
	for _, row := range rows {
		listed, known := rooms[row.RoomID]
		if !known || asked[row.RoomID] || (row.Platform == "discord" && row.NetworkID != "") ||
			(run.selfPerson != "" && people[row.SenderUserID] == run.selfPerson) || self[row.SenderUserID] {
			continue
		}
		member := strings.HasSuffix(strings.SplitN(row.SenderUserID, ":", 2)[0], "bot")
		for _, candidate := range listed.Members {
			member = member || candidate.UserID == row.SenderUserID
		}
		if member {
			continue
		}
		asked[row.RoomID] = true
		var page struct {
			Messages []struct {
				Sender string `json:"sender"`
				IsMe   bool   `json:"is_me"`
			} `json:"messages"`
		}
		if err := run.multichat.get("/conversations/"+url.PathEscape(row.RoomID)+"/messages", url.Values{"limit": {"200"}}, &page); err != nil {
			return nil, err
		}
		for _, message := range page.Messages {
			if message.IsMe {
				self[message.Sender] = true
			}
		}
	}
	return self, nil
}

// photoIndex lists photo-store's live photos that came from multichat, by
// every message-log ref photo-store found each in: one picture sent in two
// messages belongs to both. A private document is left out: it is never
// linked.
func (run runner) photoIndex() (map[string][]photoRef, error) {
	index := map[string][]photoRef{}
	for offset := 0; ; offset += 1000 {
		var page []struct {
			PhotoID    string   `json:"photo_id"`
			SourcePath string   `json:"source_path"`
			MediaType  string   `json:"media_type"`
			Tags       []string `json:"tags"`
		}
		query := url.Values{"source_machine": {photoSourceMachine}, "limit": {"1000"}, "offset": {fmt.Sprint(offset)}}
		if _, err := getJSON(run.photoStoreURL+"/sightings?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, sighting := range page {
			if isPrivate(sighting.Tags) {
				continue
			}
			index[sighting.SourcePath] = append(index[sighting.SourcePath], photoRef{ID: sighting.PhotoID, MediaType: sighting.MediaType})
		}
		if len(page) < 1000 {
			break
		}
	}
	for ref := range index {
		sort.SliceStable(index[ref], func(i, j int) bool { return index[ref][i].ID < index[ref][j].ID })
	}
	return index, nil
}

// photoSourceMachine is the source_machine photo-store gives a picture
// imported from multichat's log; its source_path is the row's ref.
const photoSourceMachine = "multichat"

func isPrivate(tags []string) bool {
	for _, tag := range tags {
		if tag == photolookup.PrivateDocumentTag {
			return true
		}
	}
	return false
}
