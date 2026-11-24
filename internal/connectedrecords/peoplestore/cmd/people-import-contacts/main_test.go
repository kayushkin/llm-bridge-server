package main

import (
	"strings"
	"testing"

	peoplestore "github.com/kayushkin/people-store"
)

const allContacts = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice Example\r\nN:Example;Alice;;;\r\n" +
	"TEL;TYPE=CELL:(202) 555-0100\r\nitem1.EMAIL;TYPE=INTERNET:Alice@Example.com\r\nitem1.X-ABLabel:Home\r\n" +
	"BDAY:--0229\r\nNOTE:met at the\\, um\\, park\\nlong ago\r\nCATEGORIES:myContacts,starred\r\n" +
	"PHOTO:https://example.com/photos/alice-with-a-very-long-address-that-a-writer-would-\r\n fold\r\nEND:VCARD\r\n" +
	"BEGIN:VCARD\r\nVERSION:3.0\r\nitem1.EMAIL;TYPE=INTERNET:recruiter@example.org\r\nitem1.X-ABLabel:\r\nEND:VCARD\r\n" +
	"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Mom\r\nN:;Mom;;;\r\nTEL;TYPE=CELL:+1 202-555-0101\r\nCATEGORIES:myContacts\r\nEND:VCARD\r\n" +
	"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Carrier Help\r\nTEL:*729\r\nTEL:611\r\nCATEGORIES:myContacts\r\nEND:VCARD\r\n"

// The starred label's file repeats Alice with the number in another spelling.
const starredLabel = "BEGIN:VCARD\nVERSION:4.0\nFN:Alice Example\nTEL;VALUE=uri:tel:+1-202-555-0100\nCATEGORIES:myContacts,starred\nEND:VCARD\n"

func readTestCards(t *testing.T, rule phoneRule) []contact {
	t.Helper()
	var contacts []contact
	for file, text := range map[string]string{"All Contacts/All Contacts.vcf": allContacts, "Starred/Starred.vcf": starredLabel} {
		cards, err := parseVCards(strings.NewReader(text))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, card := range cards {
			contacts = append(contacts, contactFromCard(card, file, rule))
		}
	}
	return contacts
}

func TestCardsAreUnfoldedUnescapedAndReadWithTheirLabels(t *testing.T) {
	contacts := readTestCards(t, phoneRule{NorthAmerican: true})
	var alice *contact
	for index := range contacts {
		if contacts[index].FormattedName == "Alice Example" && len(contacts[index].Emails) > 0 {
			alice = &contacts[index]
		}
	}
	if alice == nil {
		t.Fatal("no Alice card with an email")
	}
	if alice.Note != "met at the, um, park\nlong ago" || alice.Birthday != "--02-29" || !alice.Starred || !alice.HasPhoto || alice.StructuredName != "Alice Example" {
		t.Errorf("alice = %+v", alice)
	}
	if len(alice.Emails) != 1 || alice.Emails[0].Address != "alice@example.com" || alice.Emails[0].Label != "Home" {
		t.Errorf("emails = %+v", alice.Emails)
	}
	if len(alice.Phones) != 1 || alice.Phones[0].E164 != "+12025550100" || alice.Phones[0].Raw != "(202) 555-0100" || alice.Phones[0].Label != "cell" {
		t.Errorf("phones = %+v", alice.Phones)
	}
}

func TestAVersionTwoCardIsRefusedRatherThanMisread(t *testing.T) {
	_, err := parseVCards(strings.NewReader("BEGIN:VCARD\nVERSION:2.1\nFN:Bob\nEND:VCARD\n"))
	if err == nil || !strings.Contains(err.Error(), "2.1") {
		t.Errorf("parse = %v, want a refusal naming the version", err)
	}
}

func TestPhonesBecomeE164OnlyWhenTheCountryIsKnown(t *testing.T) {
	cases := []struct {
		raw           string
		northAmerican bool
		want          string
	}{
		{"+1 202-555-0100", false, "+12025550100"},
		{"+44 20 7946 0958", false, "+442079460958"},
		{"(202) 555-0100", false, ""},
		{"(202) 555-0100", true, "+12025550100"},
		{"1 202-555-0100", true, "+12025550100"},
		{"(202)5550100", true, "+12025550100"},
		{"611", true, ""},
		{"*729", true, ""},
		{"*3282#", true, ""},
		{"(102) 555-0100", true, ""},
		{"020 7946 0958", true, ""},
	}
	for _, test := range cases {
		got, skipped := phoneRule{NorthAmerican: test.northAmerican}.normalize(test.raw)
		if got != test.want || (got == "") == (skipped == "") {
			t.Errorf("normalize(%q, north american %v) = %q, %q; want %q", test.raw, test.northAmerican, got, skipped, test.want)
		}
	}
}

func TestBirthdaysTakeTheFormsVCardWrites(t *testing.T) {
	for raw, want := range map[string]string{"--0229": "--02-29", "--02-29": "--02-29", "1990-05-01": "1990-05-01", "19900501": "1990-05-01",
		"1990-05-01T00:00:00Z": "1990-05-01", "--1340": "", "1990-02-30": "", "soon": ""} {
		if got := normalizeBirthday(raw); got != want {
			t.Errorf("normalizeBirthday(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNamesMatchOnTheSameWordsAndOneWordMatchesOnlyItself(t *testing.T) {
	cases := []struct {
		contact, person string
		want            bool
	}{
		{"Alice Example", "alice example", true},
		{"Example, Alice", "Alice Example", true},
		{"Alice B. Example", "Alice Example", true},
		{"Alice Example", "Alice Q Jones Example", true},
		{"Alice Example", "Alice Other", false},
		{"Alice", "Alice Example", false},
		{"Mom", "Alice Example", false},
		{"Mom", "Mom", true},
		{"Alice", "Alice", true},
		{"Alice 2", "Alice", false},
		{"", "Alice", false},
	}
	for _, test := range cases {
		if got := namesPlausiblyMatch(test.contact, test.person); got != test.want {
			t.Errorf("namesPlausiblyMatch(%q, %q) = %v, want %v", test.contact, test.person, got, test.want)
		}
	}
}

func TestCardsForOnePersonAreMergedAndStrangersSharingANumberAreNot(t *testing.T) {
	contacts := readTestCards(t, phoneRule{NorthAmerican: true})
	contacts = append(contacts, contact{FormattedName: "Bob Example", Phones: []contactPhone{{Raw: "x", E164: "+12025550100"}}, Categories: []string{ownContactsCategory}, Files: []string{"b.vcf"}})
	merged := mergeContacts(contacts)
	byName := map[string][]contact{}
	for _, one := range merged {
		byName[one.displayName()] = append(byName[one.displayName()], one)
	}
	alice := byName["Alice Example"]
	if len(alice) != 1 || len(alice[0].Files) != 2 || len(alice[0].Phones) != 1 || len(alice[0].Emails) != 1 {
		t.Fatalf("alice merged = %+v", alice)
	}
	if len(byName["Bob Example"]) != 1 {
		t.Errorf("Bob, who shares Alice's number, was merged into her: %+v", byName)
	}
}

func TestThePlanSkipsWhatIsKnownAndOnlyHintsAtANameMatch(t *testing.T) {
	contacts := readTestCards(t, phoneRule{NorthAmerican: true})
	contacts = append(contacts,
		contact{FormattedName: "Carol Example", Phones: []contactPhone{{E164: "+12025550102"}}, Emails: []contactEmail{{Address: "carol@example.com"}}, Categories: []string{ownContactsCategory}, Birthday: "1980-01-02", BirthdayRaw: "1980-01-02"},
		contact{FormattedName: "Dan Example", Phones: []contactPhone{{E164: "+12025550103"}}, Categories: []string{ownContactsCategory}},
		contact{FormattedName: "Erin Example", Phones: []contactPhone{{E164: "+12025550104"}}, Categories: []string{ownContactsCategory}},
	)
	state := storeState{
		persons: map[string]peoplestore.Person{
			"person_000001": {ID: "person_000001", DisplayName: "Alice Example"},
			"person_000002": {ID: "person_000002", DisplayName: "Carol Example"},
			"person_000003": {ID: "person_000003", DisplayName: "Dan Example", Birthday: "--03-04"},
			"person_000004": {ID: "person_000004", DisplayName: "Momo Stranger"},
		},
		owners: map[string]string{
			handleKey("phone", "+12025550102"): "person_000002",
			handleKey("phone", "+12025550103"): "person_000003",
		},
		suggested: map[string]string{handleKey("phone", "+12025550104"): "suggestion_000009"},
	}
	plan := planImport(contacts, state, false)
	byName := map[string]plannedContact{}
	for _, item := range plan {
		byName[item.Contact.displayName()] = item
	}

	alice := byName["Alice Example"]
	if alice.Outcome != outcomeSuggest || alice.Kind != kindNameMatch || len(alice.NameMatches) != 1 || alice.NameMatches[0] != "person_000001" || len(alice.Owners) != 0 {
		t.Errorf("alice = %+v", alice)
	}
	if !strings.Contains(alice.Input.Reason, "Starred") || !strings.Contains(alice.Input.Reason, "person_000001") || alice.Input.Source != peoplestore.SuggestionSourceGoogleContacts {
		t.Errorf("alice's reason = %q", alice.Input.Reason)
	}
	carol := byName["Carol Example"]
	if carol.Outcome != outcomeSuggest || carol.Kind != kindExtends || len(carol.New) != 1 || carol.New[0].Value != "carol@example.com" || len(carol.Input.Identities) != 2 {
		t.Errorf("carol = %+v; want her email proposed with her owned phone sent along", carol)
	}
	if len(carol.NameMatches) != 0 || !strings.Contains(carol.Input.Reason, "1980-01-02") {
		t.Errorf("carol: name matches %v, reason %q", carol.NameMatches, carol.Input.Reason)
	}
	dan := byName["Dan Example"]
	if dan.Outcome != outcomeKnown || dan.BirthdayNote != "" {
		t.Errorf("dan = %+v", dan)
	}
	if erin := byName["Erin Example"]; erin.Outcome != outcomeAlreadyProposed {
		t.Errorf("erin = %+v", erin)
	}
	mom := byName["Mom"]
	if mom.Outcome != outcomeSuggest || mom.Kind != kindNewPerson || len(mom.NameMatches) != 0 {
		t.Errorf("mom = %+v; want a new-person suggestion matched to nobody", mom)
	}
	if carrier := byName["Carrier Help"]; carrier.Outcome != outcomeNoHandle {
		t.Errorf("carrier = %+v", carrier)
	}
	other := 0
	for _, item := range plan {
		if item.Outcome == outcomeOtherContact {
			other++
		}
	}
	if other != 1 {
		t.Errorf("%d other contacts skipped, want the recruiter's address only", other)
	}
	if with := planImport(contacts, state, true); len(with) != len(plan) {
		t.Errorf("including other contacts changed the contact count: %d vs %d", len(with), len(plan))
	}
}

func TestANumberListedTwiceIsOneHandle(t *testing.T) {
	card := contact{FormattedName: "Bob", Phones: []contactPhone{{Raw: "+1 202 555 0100", E164: "+12025550100"}, {Raw: "+1 (202) 555-0100", E164: "+12025550100"}}}
	if handles := card.handles(); len(handles) != 1 {
		t.Errorf("handles = %+v, want one", handles)
	}
}
