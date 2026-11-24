// Command people-import-contacts proposes people-store suggestions from a
// Google Contacts export: the vCard (.vcf) files Google Takeout writes, one
// per contact label, in which one contact appears in several files.
//
// It only proposes; people-store attaches nothing until the operator accepts
// a suggestion on dash's People page. For each contact, merged across files:
//
//   - every phone and email already attached to a person: nothing to do, and
//     the contact is counted as known;
//   - some handles attached to a person and some not: one suggestion holding
//     them all, which people-store trims to the new ones and points at the
//     person who has the rest (matched_person_ids);
//   - no handle attached, but a name that plausibly matches a person: one
//     suggestion whose reason names that person, so the operator can merge
//     into them; nothing is ever linked on a name;
//   - nothing matching: one suggestion, which accepted makes a new person.
//
// A starred contact says so in its reason. A birthday is only ever proposed,
// in the reason and source detail, never written to a person. Contact photos
// are left alone: people-store has no place for a photo.
//
// The source is an importer (google_contacts): people-store never proposes a
// handle any suggestion already holds, so a second run proposes only what is
// new and a rejected suggestion is not proposed again.
//
// Settings come from the environment, declared in
// peoplestore.ContactsImporterSettingDefinitions:
//
//	PEOPLE_STORE_URL  people-store's base URL (http://127.0.0.1:8321)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	peoplestore "github.com/kayushkin/people-store"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func main() {
	exportDirectory := flag.String("export-dir", "", "the Takeout Contacts directory; every .vcf under it is read (required)")
	northAmerican := flag.Bool("north-american-numbers", false, "read a phone number written without a country code as a North American (+1) number; without this such numbers are skipped")
	includeOtherContacts := flag.Bool("include-other-contacts", false, "also propose Google's \"Other contacts\": addresses Gmail kept that the owner never saved")
	dryRun := flag.Bool("dry-run", false, "write nothing; print what would be proposed")
	sampleSize := flag.Int("sample", 5, "how many examples of each outcome to print")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: people-import-contacts -export-dir DIR [-north-american-numbers] [-include-other-contacts] [-dry-run]\n\nProposes a people-store suggestion for each Google contact with phones or emails nobody has yet.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *exportDirectory == "" {
		flag.Usage()
		os.Exit(2)
	}

	settings, err := peoplestore.NewContactsImporterSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		fail("%v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		fail("%v", err)
	}
	store := storeClient{baseURL: strings.TrimRight(settings.String(peoplestore.ImporterSettingPeopleStoreURL), "/")}

	cards, err := readExport(*exportDirectory, phoneRule{NorthAmerican: *northAmerican})
	if err != nil {
		fail("%v", err)
	}
	state, err := store.loadState()
	if err != nil {
		fail("%v", err)
	}
	plan := planImport(cards, state, *includeOtherContacts)

	report := newReport(*sampleSize)
	report.CardsRead = len(cards)
	report.Contacts = len(plan)
	for _, item := range plan {
		if item.Outcome != outcomeSuggest || *dryRun {
			report.add(item, nil)
			continue
		}
		proposal, err := store.propose(item.Input)
		if err != nil {
			report.Failed++
			fmt.Fprintf(os.Stderr, "people-import-contacts: %q: %v\n", item.Input.DisplayName, err)
			continue
		}
		report.add(item, proposal)
	}
	report.print(os.Stdout, *dryRun)
	if report.Failed > 0 {
		fail("%d contacts failed", report.Failed)
	}
}

// readExport parses every .vcf under directory, in path order.
func readExport(directory string, rule phoneRule) ([]contact, error) {
	var paths []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".vcf") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .vcf file under %s", directory)
	}
	sort.Strings(paths)
	var contacts []contact
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		cards, err := parseVCards(file)
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return nil, err
		}
		for _, card := range cards {
			contacts = append(contacts, contactFromCard(card, relative, rule))
		}
	}
	return contacts, nil
}

// storeState is what people-store already knows that the plan reads.
type storeState struct {
	persons map[string]peoplestore.Person // live persons by id
	// owners maps "kind\x00value" to the person who has that handle.
	owners map[string]string
	// suggested maps "kind\x00value" to a suggestion that holds it, whatever
	// its status: people-store never lets an importer propose it again.
	suggested map[string]string
}

func handleKey(kind, value string) string { return kind + "\x00" + value }

type outcome string

const (
	outcomeSuggest         outcome = "suggest"
	outcomeKnown           outcome = "already_known"         // every handle is attached to a person
	outcomeAlreadyProposed outcome = "already_proposed"      // no handle is new, some sit in a suggestion
	outcomeNoHandle        outcome = "skipped_no_handle"     // no phone or email people-store can store
	outcomeOtherContact    outcome = "skipped_other_contact" // Gmail's "Other contacts"
	outcomeNoName          outcome = "skipped_no_name"       // nothing to call the person
)

// Kinds of suggestion, for the report.
const (
	kindExtends   = "extends_person" // some handles belong to a person already
	kindNameMatch = "name_match"     // no handle known; the name matches a person
	kindNewPerson = "new_person"     // nothing matches
)

type plannedContact struct {
	Contact contact
	Outcome outcome
	Kind    string
	Input   peoplestore.SuggestionInput
	// New are the handles the store would keep; Owners the persons who own
	// the rest; NameMatches persons whose name matches.
	New         []peoplestore.IdentityInput
	Owners      []string
	NameMatches []string
	// BirthdayNote says what became of the card's birthday.
	BirthdayNote string
}

// planImport decides, for each contact, what the store would be asked. It
// reads the same rules ProposeSuggestion applies so that a dry run says what
// a real run will do, and claims each new handle as it goes, as the store
// does, so two contacts sharing a number are proposed once.
func planImport(cards []contact, state storeState, includeOtherContacts bool) []plannedContact {
	var plan []plannedContact
	claimed := map[string]bool{}
	for _, card := range mergeContacts(cards) {
		item := plannedContact{Contact: card}
		handles := card.handles()
		switch {
		case card.isOtherContact() && !includeOtherContacts:
			item.Outcome = outcomeOtherContact
		case len(handles) == 0:
			item.Outcome = outcomeNoHandle
		case card.displayName() == "":
			item.Outcome = outcomeNoName
		}
		if item.Outcome != "" {
			plan = append(plan, item)
			continue
		}
		owners := map[string]bool{}
		suggestedSomewhere := false
		for _, handle := range handles {
			key := handleKey(handle.Kind, handle.Value)
			if owner, owned := state.owners[key]; owned {
				owners[owner] = true
				continue
			}
			if _, inSuggestion := state.suggested[key]; inSuggestion || claimed[key] {
				suggestedSomewhere = true
				continue
			}
			item.New = append(item.New, handle)
		}
		for owner := range owners {
			item.Owners = append(item.Owners, owner)
		}
		sort.Strings(item.Owners)
		item.NameMatches = nameMatches(card, state.persons, owners)
		item.BirthdayNote = birthdayNote(card, item.Owners, state.persons)
		if len(item.New) == 0 {
			item.Outcome = outcomeKnown
			if suggestedSomewhere {
				item.Outcome = outcomeAlreadyProposed
			}
			plan = append(plan, item)
			continue
		}
		for _, handle := range item.New {
			claimed[handleKey(handle.Kind, handle.Value)] = true
		}
		item.Outcome = outcomeSuggest
		switch {
		case len(item.Owners) > 0:
			item.Kind = kindExtends
		case len(item.NameMatches) > 0:
			item.Kind = kindNameMatch
		default:
			item.Kind = kindNewPerson
		}
		item.Input = suggestionInput(item, state.persons)
		plan = append(plan, item)
	}
	return plan
}

// nameMatches are the live persons, other than those who already own one of
// the contact's handles, whose name plausibly matches one of the contact's.
func nameMatches(card contact, persons map[string]peoplestore.Person, owners map[string]bool) []string {
	var matches []string
	for id, person := range persons {
		if owners[id] {
			continue
		}
		for _, name := range card.names() {
			if namesPlausiblyMatch(name, person.DisplayName) {
				matches = append(matches, id)
				break
			}
		}
	}
	sort.Strings(matches)
	return matches
}

func birthdayNote(card contact, owners []string, persons map[string]peoplestore.Person) string {
	if card.BirthdayRaw == "" {
		return ""
	}
	if card.Birthday == "" {
		return fmt.Sprintf("The card's birthday %q is not a date people-store reads.", card.BirthdayRaw)
	}
	for _, owner := range owners {
		if existing := persons[owner].Birthday; existing != "" && existing != card.Birthday {
			return fmt.Sprintf("The card says birthday %s; %s has %s, which nothing here changes.", card.Birthday, owner, existing)
		}
		if persons[owner].Birthday == card.Birthday {
			return ""
		}
	}
	return fmt.Sprintf("The card says birthday %s. Accepting does not set it: set it on the person if it is right.", card.Birthday)
}

// sourceDetail is what the suggestion keeps of the card for a reviewer.
type sourceDetail struct {
	FormattedName  string         `json:"formatted_name,omitempty"`
	StructuredName string         `json:"structured_name,omitempty"`
	Nicknames      []string       `json:"nicknames,omitempty"`
	Organization   string         `json:"organization,omitempty"`
	Birthday       string         `json:"birthday,omitempty"`
	BirthdayRaw    string         `json:"birthday_raw,omitempty"`
	Note           string         `json:"note,omitempty"`
	Categories     []string       `json:"categories,omitempty"`
	Starred        bool           `json:"starred"`
	HasPhoto       bool           `json:"has_photo"`
	Phones         []contactPhone `json:"phones,omitempty"`
	Emails         []contactEmail `json:"emails,omitempty"`
	Files          []string       `json:"files"`
	NameMatches    []nameMatch    `json:"name_matches,omitempty"`
}

type nameMatch struct {
	PersonID    string `json:"person_id"`
	DisplayName string `json:"display_name"`
}

// suggestionInput builds the POST /suggestions body. It sends every handle,
// owned ones too: people-store leaves those out and names their owners in
// matched_person_ids, which is how dash shows "some handles already belong
// to".
func suggestionInput(item plannedContact, persons map[string]peoplestore.Person) peoplestore.SuggestionInput {
	card := item.Contact
	var reason []string
	reason = append(reason, fmt.Sprintf("Google Contacts card %q.", card.displayName()))
	if card.Starred {
		reason = append(reason, "Starred on the phone.")
	}
	if len(item.NameMatches) > 0 {
		var named []string
		for _, id := range item.NameMatches {
			named = append(named, fmt.Sprintf("%s (%s)", id, persons[id].DisplayName))
		}
		reason = append(reason, fmt.Sprintf("No handle is known, but the name matches %s: if it is them, merge into them rather than accept as new.", strings.Join(named, ", ")))
	}
	if card.Organization != "" {
		reason = append(reason, fmt.Sprintf("Organization: %s.", card.Organization))
	}
	if item.BirthdayNote != "" {
		reason = append(reason, item.BirthdayNote)
	}
	detail := sourceDetail{
		FormattedName: card.FormattedName, StructuredName: card.StructuredName, Nicknames: card.Nicknames, Organization: card.Organization,
		Birthday: card.Birthday, BirthdayRaw: card.BirthdayRaw, Note: card.Note, Categories: card.Categories, Starred: card.Starred,
		HasPhoto: card.HasPhoto, Phones: card.Phones, Emails: card.Emails, Files: card.Files,
	}
	for _, id := range item.NameMatches {
		detail.NameMatches = append(detail.NameMatches, nameMatch{PersonID: id, DisplayName: persons[id].DisplayName})
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		panic(err) // only strings, bools and slices of them
	}
	return peoplestore.SuggestionInput{
		Source:       peoplestore.SuggestionSourceGoogleContacts,
		DisplayName:  card.displayName(),
		Identities:   card.handles(),
		SourceDetail: encoded,
		Reason:       strings.Join(reason, " "),
	}
}

// report counts outcomes and keeps a few examples of each.
type report struct {
	sampleSize int
	CardsRead  int
	Contacts   int
	Outcomes   map[outcome]int
	Kinds      map[string]int
	Starred    int
	// Proposed counts suggestions the store made; NothingNew those it
	// answered created:false for, which the plan did not foresee.
	Proposed, NothingNew, Failed int
	PhonesSkipped                map[string]int
	samples                      map[string][]string
}

func newReport(sampleSize int) *report {
	return &report{sampleSize: sampleSize, Outcomes: map[outcome]int{}, Kinds: map[string]int{}, PhonesSkipped: map[string]int{}, samples: map[string][]string{}}
}

func (r *report) add(item plannedContact, proposal *peoplestore.SuggestionProposal) {
	r.Outcomes[item.Outcome]++
	for _, phone := range item.Contact.Phones {
		if phone.Skipped != "" {
			r.PhonesSkipped[phone.Skipped]++
		}
	}
	bucket := string(item.Outcome)
	line := item.Contact.displayName()
	if item.Outcome == outcomeSuggest {
		r.Kinds[item.Kind]++
		if item.Contact.Starred {
			r.Starred++
		}
		bucket = item.Kind
		var handles []string
		for _, handle := range item.New {
			handles = append(handles, handle.Kind+" "+handle.Value)
		}
		line = fmt.Sprintf("%s [%s] owners=%v name_matches=%v — %s", line, strings.Join(handles, ", "), item.Owners, item.NameMatches, item.Input.Reason)
		if proposal != nil {
			if proposal.Created {
				r.Proposed++
				line = proposal.Suggestion.ID + "  " + line
				fmt.Printf("%s  %s (%d handles)\n", proposal.Suggestion.ID, item.Input.DisplayName, len(proposal.Suggestion.Payload.Identities))
			} else {
				r.NothingNew++
			}
		}
	} else {
		if len(item.Owners) > 0 {
			line += " owners=" + strings.Join(item.Owners, ",")
		}
		if item.BirthdayNote != "" {
			line += " — " + item.BirthdayNote
			bucket += "_with_birthday"
		}
	}
	if len(r.samples[bucket]) < r.sampleSize || strings.HasSuffix(bucket, "_with_birthday") {
		r.samples[bucket] = append(r.samples[bucket], line)
	}
}

func (r *report) print(out io.Writer, dryRun bool) {
	summary := map[string]any{
		"dry_run": dryRun, "cards_read": r.CardsRead, "contacts_after_merge": r.Contacts,
		"outcomes": r.Outcomes, "suggestion_kinds": r.Kinds, "starred_suggestions": r.Starred,
		"phones_skipped": r.PhonesSkipped,
	}
	if !dryRun {
		summary["suggestions_created"] = r.Proposed
		summary["nothing_new_at_store"] = r.NothingNew
		summary["failed"] = r.Failed
	}
	var buckets []string
	for bucket := range r.samples {
		buckets = append(buckets, bucket)
	}
	sort.Strings(buckets)
	for _, bucket := range buckets {
		fmt.Fprintf(out, "== %s\n", bucket)
		for _, line := range r.samples[bucket] {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		fail("%v", err)
	}
	fmt.Fprintln(out, string(encoded))
}

// storeClient is people-store's HTTP API.
type storeClient struct{ baseURL string }

// pageSize is the most people-store's list routes answer at once.
const pageSize = 1000

func (c storeClient) loadState() (storeState, error) {
	state := storeState{persons: map[string]peoplestore.Person{}, owners: map[string]string{}, suggested: map[string]string{}}
	for offset := 0; ; offset += pageSize {
		var page []peoplestore.Person
		if err := c.get("/persons", url.Values{"limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}, &page); err != nil {
			return state, err
		}
		for _, person := range page {
			state.persons[person.ID] = person
		}
		if len(page) < pageSize {
			break
		}
	}
	for _, kind := range []string{peoplestore.IdentityKindPhone, peoplestore.IdentityKindEmail} {
		for offset := 0; ; offset += pageSize {
			var page []peoplestore.Identity
			if err := c.get("/identities", url.Values{"kind": {kind}, "limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}, &page); err != nil {
				return state, err
			}
			for _, identity := range page {
				state.owners[handleKey(identity.Kind, identity.Value)] = identity.PersonID
			}
			if len(page) < pageSize {
				break
			}
		}
	}
	for offset := 0; ; offset += pageSize {
		var page []peoplestore.Suggestion
		if err := c.get("/suggestions", url.Values{"limit": {fmt.Sprint(pageSize)}, "offset": {fmt.Sprint(offset)}}, &page); err != nil {
			return state, err
		}
		for _, suggestion := range page {
			for _, identity := range suggestion.Payload.Identities {
				state.suggested[handleKey(identity.Kind, identity.Value)] = suggestion.ID
			}
			for _, covered := range suggestion.Payload.Covered {
				state.suggested[handleKey(covered.Kind, covered.Value)] = suggestion.ID
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	return state, nil
}

func (c storeClient) get(path string, query url.Values, target any) error {
	response, err := httpClient.Get(c.baseURL + path + "?" + query.Encode())
	if err != nil {
		return fmt.Errorf("people-store: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("people-store: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("people-store: GET %s answered %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("people-store: GET %s: %w", path, err)
	}
	return nil
}

func (c storeClient) propose(input peoplestore.SuggestionInput) (*peoplestore.SuggestionProposal, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	response, err := httpClient.Post(c.baseURL+"/suggestions", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("POST /suggestions answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var proposal peoplestore.SuggestionProposal
	if err := json.Unmarshal(body, &proposal); err != nil {
		return nil, err
	}
	return &proposal, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "people-import-contacts: "+format+"\n", args...)
	os.Exit(1)
}
