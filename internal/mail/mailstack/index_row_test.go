package mailstack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
)

// These pin the call site, not the helpers.
//
// backend/identity_test.go already covers HeaderLookup, StripAngleBrackets and
// SplitReferences one at a time, and covers them well. What it cannot reach is
// the place those three are composed into the row the index actually stores:
// indexRow, rootMessageID and providerIDScope all live in the root mailstack package,
// which had no test file at all. Measured before this file existed, ten mutations
// aimed at that composition — reading the newest ancestor instead of the
// oldest, dropping the In-Reply-To fallback, swapping two arguments of the same
// type, scoping every provider by folder — left the suite green, every one.
//
// The helpers being correct is what makes this worth pinning rather than
// reassuring: a thorough suite one layer down is exactly what makes the
// unheld layer look covered.

// oneAccount builds the account argument. Provider is the only field indexRow
// reads through providerIDScope, so it is the only one varied.
func oneAccount(provider accounts.Provider) *accounts.Account {
	return &accounts.Account{ID: "acct-1", Provider: provider}
}

func TestRootMessageIDPrefersTheChainRootOverTheParentAndTheSelf(t *testing.T) {
	// The three tiers are ordered, and the order is the whole point: References
	// is oldest first per RFC 5322 §3.6.4, so its FIRST entry is the thread
	// root, In-Reply-To is only the parent, and a message with neither is its
	// own root. Every fixture below keeps the three candidate answers distinct,
	// because a chain whose root equals its parent cannot fail for a wrong pick.
	cases := []struct {
		name      string
		messageID string
		inReplyTo string
		refs      []string
		want      string
	}{
		{
			// Three distinct ancestors, and In-Reply-To carrying the LAST of
			// them, which is what a real reply looks like. That is what makes
			// this one fixture separate the root from the parent twice over:
			// reading refs from the wrong end and preferring In-Reply-To both
			// land on "parent", and neither is the answer.
			name:      "full chain: the oldest ancestor wins, not the newest and not the parent",
			messageID: "self@x.com",
			inReplyTo: "parent@x.com",
			refs:      []string{"root@x.com", "middle@x.com", "parent@x.com"},
			want:      "root@x.com",
		},
		{
			// A two-entry chain is the shortest one that can tell the ends
			// apart. A one-entry chain reads the same from either end, so it
			// would pass for a rule that is exactly backwards.
			name:      "two-entry chain still reads from the front",
			messageID: "self@x.com",
			inReplyTo: "",
			refs:      []string{"root@x.com", "parent@x.com"},
			want:      "root@x.com",
		},
		{
			// The boundary of `len(refs) > 0`, and the only case that can hold
			// it: a chain of exactly one is still a chain. With 3, 2 and 0
			// present but not 1, a guard reading `> 1` passes the whole table
			// while silently demoting every single-ancestor reply to its parent
			// or to itself — the commonest thread shape there is.
			name:      "a chain of exactly one entry is still a chain",
			messageID: "self@x.com",
			inReplyTo: "parent@x.com",
			refs:      []string{"root@x.com"},
			want:      "root@x.com",
		},
		{
			// IMAP's ENVELOPE carries In-Reply-To without References, so this
			// tier is a live path and not a defensive branch.
			name:      "no chain: the parent is the best root available",
			messageID: "self@x.com",
			inReplyTo: "parent@x.com",
			refs:      nil,
			want:      "parent@x.com",
		},
		{
			name:      "no ancestors at all: the message is its own root",
			messageID: "self@x.com",
			inReplyTo: "",
			refs:      nil,
			want:      "self@x.com",
		},
		{
			// Message-ID is OPTIONAL per RFC 5322 §3.6.4. A message with
			// nothing to key on yields an empty root rather than something
			// invented, and the index stores that as absent.
			name:      "nothing to key on yields an empty root",
			messageID: "",
			inReplyTo: "",
			refs:      nil,
			want:      "",
		},
	}
	for _, c := range cases {
		if got := rootMessageID(c.messageID, c.inReplyTo, c.refs); got != c.want {
			t.Errorf("%s: rootMessageID(%q, %q, %#v) = %q, want %q",
				c.name, c.messageID, c.inReplyTo, c.refs, got, c.want)
		}
	}
}

func TestProviderIDScopeIsolatesIMAPFoldersAndOnlyIMAPFolders(t *testing.T) {
	// A provider id is unique per account on Gmail and Graph, so scoping it by
	// folder would split one message into a new row on every relabel. IMAP's
	// UID is only unique within one mailbox, so NOT scoping it there collides
	// two different messages onto one row. The two halves fail in opposite
	// directions, which is why both are asserted.
	if got := providerIDScope(accounts.ProviderIMAP, "INBOX"); got != "INBOX" {
		t.Errorf("IMAP: got %q, want INBOX — an IMAP UID is only unique within its mailbox", got)
	}
	for _, p := range []accounts.Provider{accounts.ProviderGoogle, accounts.ProviderMicrosoft} {
		if got := providerIDScope(p, "INBOX"); got != "" {
			t.Errorf("%s: got %q, want empty — its provider id is already account-unique", p, got)
		}
	}
}

func TestIndexRowPrefersHeaderIdentityOverTheThinnerListValues(t *testing.T) {
	// The get path carries real headers and the list path does not, so indexRow
	// takes headers when they are there. Every header below disagrees with the
	// meta field it overrides, because a fixture where the two agree cannot
	// show which one was stored.
	meta := backend.MessageMeta{
		ID:         "uid-9",
		FolderID:   "INBOX",
		Subject:    "Re: the jacket",
		MessageID:  "from-the-list-path@x.com",
		InReplyTo:  "list-parent@x.com",
		References: []string{"list-root@x.com"},
		Date:       time.UnixMilli(1723406400000).UTC(),
		From:       backend.Address{Name: "Seller", Email: "seller@example.com"},
	}
	headers := map[string]string{
		"Message-ID":  "<header-self@x.com>",
		"In-Reply-To": "<header-parent@x.com>",
		"References":  "<header-root@x.com> <header-parent@x.com>",
	}

	row := indexRow(oneAccount(accounts.ProviderGoogle), meta, headers)

	// Angle brackets are stripped on the way in. They must be, and at this
	// layer: the index dedups one message that reached two accounts by
	// comparing these strings, and "<a@x>" never equals "a@x".
	if row.MessageID != "header-self@x.com" {
		t.Errorf("MessageID = %q, want header-self@x.com — the header value, brackets stripped", row.MessageID)
	}
	if row.InReplyTo != "header-parent@x.com" {
		t.Errorf("InReplyTo = %q, want header-parent@x.com — the header value, brackets stripped", row.InReplyTo)
	}
	wantRefs := []string{"header-root@x.com", "header-parent@x.com"}
	if !reflect.DeepEqual(row.References, wantRefs) {
		t.Errorf("References = %#v, want %#v — the header chain, not the list-path one", row.References, wantRefs)
	}
	// The root is computed from the values that won, not from the meta.
	if row.RootID != "header-root@x.com" {
		t.Errorf("RootID = %q, want header-root@x.com", row.RootID)
	}
	if row.AccountID != "acct-1" || row.ProviderID != "uid-9" {
		t.Errorf("AccountID/ProviderID = %q/%q, want acct-1/uid-9", row.AccountID, row.ProviderID)
	}
	if row.FolderScope != "" {
		t.Errorf("FolderScope = %q, want empty for a Google account", row.FolderScope)
	}
	if row.DateMs != 1723406400000 {
		t.Errorf("DateMs = %d, want 1723406400000", row.DateMs)
	}
	// The carried-through fields. They are not identity, but they are what the
	// index is read back for, and nothing else in this package holds them.
	// FromName and FromEmail are both strings on the same struct, so a swap
	// compiles; the fixture keeps them distinguishable on purpose.
	if row.Subject != "Re: the jacket" {
		t.Errorf("Subject = %q, want %q", row.Subject, "Re: the jacket")
	}
	if row.FolderID != "INBOX" {
		t.Errorf("FolderID = %q, want INBOX", row.FolderID)
	}
	if row.FromName != "Seller" || row.FromEmail != "seller@example.com" {
		t.Errorf("From = %q <%q>, want Seller <seller@example.com> — name and address are not interchangeable",
			row.FromName, row.FromEmail)
	}
}

func TestIndexRowKeepsListValuesWhenAHeaderIsAbsentRatherThanBlankingThem(t *testing.T) {
	// The non-empty guard on each header is the load-bearing part: headers is
	// non-nil here but carries none of the three, which is what a get response
	// for a message with no threading headers looks like. Without the guard,
	// having ANY headers would erase identity the list path had already found.
	meta := backend.MessageMeta{
		ID:         "uid-9",
		FolderID:   "INBOX",
		MessageID:  "from-the-list-path@x.com",
		InReplyTo:  "list-parent@x.com",
		References: []string{"list-root@x.com", "list-parent@x.com"},
	}
	headers := map[string]string{"Subject": "no threading headers here"}

	row := indexRow(oneAccount(accounts.ProviderIMAP), meta, headers)

	if row.MessageID != "from-the-list-path@x.com" {
		t.Errorf("MessageID = %q, want the list-path value kept", row.MessageID)
	}
	if row.InReplyTo != "list-parent@x.com" {
		t.Errorf("InReplyTo = %q, want the list-path value kept", row.InReplyTo)
	}
	if !reflect.DeepEqual(row.References, []string{"list-root@x.com", "list-parent@x.com"}) {
		t.Errorf("References = %#v, want the list-path chain kept", row.References)
	}
	if row.RootID != "list-root@x.com" {
		t.Errorf("RootID = %q, want list-root@x.com", row.RootID)
	}
	// The IMAP half of providerIDScope, reached through indexRow rather than
	// directly — the scope is read off the account, not off the meta.
	if row.FolderScope != "INBOX" {
		t.Errorf("FolderScope = %q, want INBOX for an IMAP account", row.FolderScope)
	}
}

func TestIndexRowLeavesAnUnknownDateAtZeroInsteadOfStampingTheEpoch(t *testing.T) {
	// time.Time's zero value is year 1, and UnixMilli on it is a large negative
	// number rather than 0 — so a missing date without the guard does not merely
	// sort oddly, it sorts before every real message forever. The pair straddles
	// the guard: one side must be 0, the other must be the real conversion.
	zero := indexRow(oneAccount(accounts.ProviderIMAP), backend.MessageMeta{ID: "uid-1"}, nil)
	if zero.DateMs != 0 {
		t.Errorf("DateMs = %d for a message with no date, want 0", zero.DateMs)
	}
	dated := indexRow(oneAccount(accounts.ProviderIMAP),
		backend.MessageMeta{ID: "uid-2", Date: time.UnixMilli(1723406400123).UTC()}, nil)
	if dated.DateMs != 1723406400123 {
		t.Errorf("DateMs = %d, want 1723406400123 — millisecond precision, not seconds", dated.DateMs)
	}
}

// newIndexedServer builds a Server backed by a real store in a temp directory.
// The index path is three SQL statements deep, so a fake store would only
// re-assert what the fake was written to do.
func newIndexedServer(t *testing.T, provider accounts.Provider) *Server {
	t.Helper()
	store, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Save(&accounts.Account{ID: "acct-1", Label: "test", Provider: provider, Address: "me@x.com"}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return NewServer(store, nil, testSettings(t, ""))
}

func TestRecordFullMessageCarriesTheHeadersThatAreTheGetPathsWholeAdvantage(t *testing.T) {
	// indexRow being pinned does not pin its callers, which is the same shape
	// one level up: recordFullMessage exists precisely because the get path has
	// headers the list path does not, and it hands them to indexRow as an
	// argument. Passing nil there compiles, keeps every indexRow test green,
	// and silently reduces the richer path to the poorer one.
	//
	// The fixture makes the two paths disagree: the header chain names an
	// ancestor the list-path meta has never heard of, so the stored root says
	// which path ran.
	s := newIndexedServer(t, accounts.ProviderGoogle)
	msg := backend.Message{
		Meta: backend.MessageMeta{
			ID:        "uid-42",
			AccountID: "acct-1",
			FolderID:  "INBOX",
			Subject:   "Re: the jacket",
			MessageID: "self@x.com",
		},
		Headers: map[string]string{
			"References": "<only-in-the-headers@x.com> <parent@x.com>",
		},
	}
	s.recordFullMessage("acct-1", msg)

	got, err := s.accounts.MessageByLocator("acct-1", "", "uid-42")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got == nil {
		t.Fatal("the message was not indexed at all")
	}
	if got.RootID != "only-in-the-headers@x.com" {
		t.Errorf("RootID = %q, want only-in-the-headers@x.com — the headers were dropped, "+
			"so the get path stored what the list path would have", got.RootID)
	}
}

func TestIndexMessagesIndexesEveryMessageOfAPageAndNotJustTheFirst(t *testing.T) {
	// The list path's loop is the other caller. Two messages, because a loop
	// that indexes only its first element satisfies a one-message fixture, and
	// they differ in provider id so the read-back can tell them apart.
	s := newIndexedServer(t, accounts.ProviderIMAP)
	s.indexMessages("acct-1", []backend.MessageMeta{
		{ID: "uid-1", FolderID: "INBOX", MessageID: "one@x.com"},
		{ID: "uid-2", FolderID: "INBOX", MessageID: "two@x.com"},
	})
	for _, want := range []struct{ providerID, messageID string }{
		{"uid-1", "one@x.com"},
		{"uid-2", "two@x.com"},
	} {
		// FolderScope is "INBOX" and not "" because this account is IMAP —
		// reading it back through the wrong scope is how a scoping regression
		// would show up here.
		got, err := s.accounts.MessageByLocator("acct-1", "INBOX", want.providerID)
		if err != nil {
			t.Fatalf("read back %s: %v", want.providerID, err)
		}
		if got == nil {
			t.Errorf("%s was not indexed", want.providerID)
			continue
		}
		if got.MessageID != want.messageID {
			t.Errorf("%s: MessageID = %q, want %q", want.providerID, got.MessageID, want.messageID)
		}
	}
}

func TestLookupByMessageIDNormalizesTheBracketedFormCallersActuallySend(t *testing.T) {
	// The third consumer of the same helpers, and the one that faces outwards.
	// The index stores Message-IDs with their angle brackets stripped, but RFC
	// 5322 writes them WITH brackets, so that is the form a caller copies out
	// of a mail client and puts in the URL. Without the normalization the
	// lookup compares "<a@x.com>" against a stored "a@x.com", matches nothing,
	// and answers 200 with indexed:false — which reads as "we have never seen
	// this message" rather than "you spelled it the documented way".
	//
	// Driven through the real mux rather than by calling the handler, because
	// the path value has to survive routing and percent-decoding to get here.
	s := newIndexedServer(t, accounts.ProviderGoogle)
	s.recordFullMessage("acct-1", backend.Message{
		Meta: backend.MessageMeta{ID: "uid-7", AccountID: "acct-1", FolderID: "INBOX", MessageID: "stored@x.com"},
	})

	for _, spelling := range []string{"stored@x.com", "<stored@x.com>"} {
		req := httptest.NewRequest("GET", "/api/lookup/message-id/"+url.PathEscape(spelling), nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 — body %s", spelling, rec.Code, rec.Body.String())
		}
		var got struct {
			MessageID string `json:"message_id"`
			Indexed   bool   `json:"indexed"`
			Locators  []struct {
				ProviderID string `json:"provider_id"`
			} `json:"locators"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decode %v — body %s", spelling, err, rec.Body.String())
		}
		// Both spellings must land on the same stored id, or the two callers
		// disagree about what they are holding.
		if got.MessageID != "stored@x.com" {
			t.Errorf("%s: echoed message_id = %q, want the normalized stored@x.com", spelling, got.MessageID)
		}
		if !got.Indexed {
			t.Errorf("%s: indexed = false, but this message was indexed above", spelling)
		}
		if len(got.Locators) != 1 || got.Locators[0].ProviderID != "uid-7" {
			t.Errorf("%s: locators = %#v, want the one uid-7 locator", spelling, got.Locators)
		}
	}
}

func TestIndexRowPassesMessageIDAndInReplyToInTheRightOrder(t *testing.T) {
	// rootMessageID takes two strings of the same type adjacent in its
	// signature, so swapping them compiles and changes the answer only when the
	// two differ AND the chain is empty. Both conditions hold here; either one
	// missing and the swap is invisible.
	row := indexRow(oneAccount(accounts.ProviderIMAP), backend.MessageMeta{
		ID:        "uid-3",
		MessageID: "self@x.com",
		InReplyTo: "parent@x.com",
	}, nil)
	if row.RootID != "parent@x.com" {
		t.Errorf("RootID = %q, want parent@x.com — the parent outranks the message's own id", row.RootID)
	}
}
