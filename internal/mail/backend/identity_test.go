package backend

import (
	"reflect"
	"testing"
)

// These cover the message-identity plumbing, which is otherwise nearly
// unobservable on this host: the mailbox is ~93% bulk mail, and a scan of the
// 40 newest Gmail messages found not one carrying References or In-Reply-To. A
// live check therefore proves nothing, and a regression here would stay hidden
// until the first real conversation arrived and silently failed to thread.

func TestHeaderLookupIsCaseInsensitiveAcrossAllThreeBackendSpellings(t *testing.T) {
	// The three backends hand back three different spellings of the same header
	// and an exact-case read works on one while silently returning nothing on
	// another: Gmail preserves the sender's casing, Graph sets "Message-ID"
	// explicitly, and IMAP's net/mail canonicalizes to "Message-Id".
	for _, spelling := range []string{"Message-ID", "Message-Id", "message-id", "MESSAGE-ID"} {
		headers := map[string]string{spelling: "<abc@example.com>"}
		if got := HeaderLookup(headers, "Message-ID"); got != "<abc@example.com>" {
			t.Errorf("spelling %q: got %q, want the header value", spelling, got)
		}
	}
	if got := HeaderLookup(map[string]string{"Subject": "hi"}, "Message-ID"); got != "" {
		t.Errorf("absent header should return empty, got %q", got)
	}
}

func TestStripAngleBracketsNormalizesOneIdentifier(t *testing.T) {
	cases := map[string]string{
		"<abc@example.com>":    "abc@example.com",
		"  <abc@example.com> ": "abc@example.com",
		"abc@example.com":      "abc@example.com",
		"":                     "",
	}
	for input, want := range cases {
		if got := StripAngleBrackets(input); got != want {
			t.Errorf("StripAngleBrackets(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSplitReferencesKeepsChainOrderOldestFirst(t *testing.T) {
	// RFC 5322 §3.6.4 orders References oldest first, and the thread root is
	// read off the front of this slice — so order is load-bearing, not cosmetic.
	raw := "<root@x.com> <middle@x.com>\r\n\t<parent@x.com>"
	got := SplitReferences(raw)
	want := []string{"root@x.com", "middle@x.com", "parent@x.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if len(SplitReferences("")) != 0 {
		t.Error("an empty header should yield no references")
	}
}

func TestGmailToMetaCarriesIdentityAndThreadingOffTheWire(t *testing.T) {
	// Guards the whole Gmail identity path at once: that threadId is read from
	// the message resource, and that the three headers added to metadataHeaders
	// are actually parsed rather than merely requested.
	g := &Gmail{accountID: "gmail-personal"}
	m := &gmailMessage{
		ID:       "19ff1d9a81882765",
		ThreadID: "thread-abc",
		Payload: gmailPayload{Headers: []gmailHeader{
			{Name: "Subject", Value: "Re: the jacket"},
			{Name: "From", Value: "Seller <seller@example.com>"},
			{Name: "Message-ID", Value: "<reply@example.com>"},
			{Name: "In-Reply-To", Value: "<parent@example.com>"},
			{Name: "References", Value: "<root@example.com> <parent@example.com>"},
		}},
	}
	meta := g.toMeta(m)

	if meta.ThreadID != "thread-abc" {
		t.Errorf("ThreadID = %q, want thread-abc — the provider thread id is being dropped again", meta.ThreadID)
	}
	if meta.MessageID != "reply@example.com" {
		t.Errorf("MessageID = %q, want reply@example.com (brackets stripped)", meta.MessageID)
	}
	if meta.InReplyTo != "parent@example.com" {
		t.Errorf("InReplyTo = %q, want parent@example.com", meta.InReplyTo)
	}
	want := []string{"root@example.com", "parent@example.com"}
	if !reflect.DeepEqual(meta.References, want) {
		t.Errorf("References = %#v, want %#v", meta.References, want)
	}
}

func TestGmailMetadataHeadersRequestTheThreadingHeaders(t *testing.T) {
	// toMeta can only parse what getMeta asked Gmail to return. Parsing and
	// requesting live in different functions, so a correct parser paired with an
	// unrequested header yields empty values and no error anywhere — exactly the
	// silent failure this pins down.
	for _, h := range []string{"Message-ID", "References", "In-Reply-To"} {
		if !gmailMetadataHeaders[h] {
			t.Errorf("metadataHeaders does not request %q, so it can never be parsed", h)
		}
	}
}

func TestParseReferencesHeaderHandlesFoldingAndCasing(t *testing.T) {
	// IMAP returns HEADER.FIELDS as raw header lines, not a bare value, and RFC
	// 5322 §2.2.3 lets a long header fold across continuation lines that begin
	// with whitespace. A parser that reads only the first line silently loses
	// every ancestor past the fold — which on a long thread is most of them.
	raw := "References: <root@x.com> <second@x.com>\r\n" +
		" <third@x.com>\r\n" +
		"\t<fourth@x.com>\r\n" +
		"\r\n"
	got := parseReferencesHeader(raw)
	want := []string{"root@x.com", "second@x.com", "third@x.com", "fourth@x.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("folded header: got %#v, want %#v", got, want)
	}

	// Servers vary on capitalization.
	if got := parseReferencesHeader("REFERENCES: <a@x.com>\r\n"); !reflect.DeepEqual(got, []string{"a@x.com"}) {
		t.Errorf("uppercase header name: got %#v", got)
	}

	// An unrelated header must not be mistaken for the chain.
	if got := parseReferencesHeader("Subject: not references\r\n"); len(got) != 0 {
		t.Errorf("unrelated header yielded %#v", got)
	}
	if got := parseReferencesHeader(""); len(got) != 0 {
		t.Errorf("empty input yielded %#v", got)
	}
}
