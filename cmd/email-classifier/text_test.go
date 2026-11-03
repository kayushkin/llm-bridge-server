package main

import (
	"encoding/json"
	"testing"
)

func TestCleanBodyTextHonorsBothFormsOfOutlookConditionalComment(t *testing.T) {
	// The two forms mean opposite things, and treating them alike loses real
	// prose in one direction or keeps Outlook-only markup in the other.
	//
	// Downlevel-HIDDEN — `<!--[if mso]> ... <![endif]-->` is a single well-formed
	// comment. Its content renders only in Outlook, so it is not what this user
	// sees in Gmail and must go.
	hidden := cleanBodyText(`Rent due. <!--[if mso]> Outlook-only table <![endif]--> Pay by the 15th.`)
	if want := "Rent due. Pay by the 15th."; hidden != want {
		t.Fatalf("hidden branch: got %q, want %q", hidden, want)
	}

	// Downlevel-REVEALED — `<!--[if !mso]><!--> ... <!--<![endif]-->` closes its
	// comment immediately so that every non-Outlook client renders the content.
	// It is ordinary visible text and must survive.
	revealed := cleanBodyText(`Keep track <!--[if !mso]><!--> of your account data <!--<![endif]--> today`)
	if want := "Keep track of your account data today"; revealed != want {
		t.Fatalf("revealed branch: got %q, want %q", revealed, want)
	}
}

func TestCleanBodyTextReducesTrackingURLsToTheirHost(t *testing.T) {
	// A Meetup invitation body measured on this host was ~95% click-tracking
	// URL. The host is the only part that carries any signal.
	raw := "You are invited: http://clicks.meetup.com/ls/click?upn=u001.NY3oBFzZ5LJDG7Ycnf-2FAp8-2FvJUYHmulqAOVKs3h2sPLMiI5s3cN6De1 now"
	got := cleanBodyText(raw)
	want := "You are invited: [link:clicks.meetup.com] now"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCleanBodyTextStripsPreviewPaddingCharacters(t *testing.T) {
	// eBay and Uber pad the preview line with hundreds of invisible runes.
	raw := "Here's a special discount for you. ͏‌ ͏‌ ͏‌ ͏‌ ͏‌ ͏‌"
	got := cleanBodyText(raw)
	want := "Here's a special discount for you."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCleanBodyTextResolvesTheEntitiesThatReachSubjectsAndBodies(t *testing.T) {
	raw := "Do you like your &#39;Seiko&#39; watch &amp; strap?"
	got := cleanBodyText(raw)
	want := "Do you like your 'Seiko' watch & strap?"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestShortenURLKeepsTheHostAndDiscardsTheCampaign(t *testing.T) {
	cases := map[string]string{
		"https://paypal.com/activity?id=99":  "[link:paypal.com]",
		"http://clicks.meetup.com/ls/click?": "[link:clicks.meetup.com]",
		"https://github.com":                 "[link:github.com]",
		"https://c.gle/AKMee0cs8zrVexXqHbXZ": "[link:c.gle]",
	}
	for input, want := range cases {
		if got := shortenURL(input); got != want {
			t.Errorf("shortenURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBestBodyTextPrefersHTMLWhenThePlainPartIsAStub(t *testing.T) {
	// HTML-first senders ship a text part that is a single "view in browser"
	// line. Preferring text unconditionally would throw away the whole message.
	em := pendingEmail{Full: &fullMessage{
		BodyText: "View this email in your browser",
		BodyHTML: "<html><body><p>Your rent payment of $1,250 is due on August 15. " +
			"Log in to the portal to pay or set up autopay before the due date.</p></body></html>",
	}}
	got := bestBodyText(em)
	if got == "View this email in your browser" {
		t.Fatalf("kept the stub text part instead of the HTML body: %q", got)
	}
	if want := "Your rent payment of $1,250 is due on August 15."; len(got) < len(want) {
		t.Fatalf("HTML body came back too short: %q", got)
	}
}

func TestBestBodyTextFallsBackToTheSnippetWhenTheMessageCouldNotBeFetched(t *testing.T) {
	// A message whose provider fetch failed reaches the model on its headers
	// alone, so Full is nil and the snippet is all there is to read.
	em := pendingEmail{Meta: messageMeta{Snippet: "Your daily digest is ready &amp; waiting"}}
	if got, want := bestBodyText(em), "Your daily digest is ready & waiting"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeTitleFoldsPunctuationButKeepsDistinctTitlesApart(t *testing.T) {
	if normalizeTitle("Pay August rent — Real Property Mgmt") != normalizeTitle("pay august rent  real property mgmt") {
		t.Fatal("punctuation and case should not distinguish two spellings of one title")
	}
	if normalizeTitle("Reply to masa_117 about item 206153166065") == normalizeTitle("Reply to masa_117 about item 999") {
		t.Fatal("two different item numbers must stay distinct; a fuzzy match here merges unrelated tasks")
	}
}

func TestColumnForActionSendsUnknownActionsToActionNeeded(t *testing.T) {
	// An action outside the vocabulary means the classifier misbehaved. Parking
	// that in No action would hide the failure permanently.
	if got := columnForAction("teleport"); got != columnActionNeeded {
		t.Fatalf("unknown action landed in %q, want %q", got, columnActionNeeded)
	}
	if got := columnForAction("none"); got != columnNoAction {
		t.Fatalf("action none landed in %q, want %q", got, columnNoAction)
	}
	if got := columnForAction("pay"); got != columnActionNeeded {
		t.Fatalf("action pay landed in %q, want %q", got, columnActionNeeded)
	}
}

func TestFilingReportAcceptsArraysReturnedAsEncodedStrings(t *testing.T) {
	// Observed live: two of three consecutive batches came back with `filings`
	// as a JSON-encoded string instead of an array. A strict decode rejected the
	// whole report, and because the same input reproduces the same reply, that
	// batch could never drain.
	raw := `{
	  "filings": "[{\"email_ref\":\"gmail-personal:abc\",\"new_card\":{\"title\":\"UNIQLO promotions\",\"category\":\"promotions\",\"action\":\"none\",\"urgency\":\"low\",\"summary\":\"s\"}}]"
	}`
	var rep filingReport
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Filings) != 1 || rep.Filings[0].EmailRef != "gmail-personal:abc" {
		t.Fatalf("filings did not decode: %+v", rep.Filings)
	}
	if rep.Filings[0].NewCard == nil || rep.Filings[0].NewCard.Title != "UNIQLO promotions" {
		t.Fatalf("inline new_card did not decode: %+v", rep.Filings[0].NewCard)
	}
}

func TestFilingReportStillAcceptsTheShapeTheSchemaAsksFor(t *testing.T) {
	raw := `{"filings":[{"email_ref":"gmail-personal:abc","card_ids":["c1"]}],"card_updates":[{"card_id":"c1","action":"file"}]}`
	var rep filingReport
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Filings) != 1 || rep.Filings[0].CardIDs[0] != "c1" {
		t.Fatalf("filings did not decode: %+v", rep.Filings)
	}
	if len(rep.CardUpdates) != 1 || rep.CardUpdates[0].Action != "file" {
		t.Fatalf("card_updates did not decode: %+v", rep.CardUpdates)
	}
}

func TestFilingReportRejectsAValueThatIsNeitherShape(t *testing.T) {
	// A number is not a recoverable mistake, and quietly treating it as an empty
	// list would drop every email in the batch while reporting success.
	var rep filingReport
	if err := json.Unmarshal([]byte(`{"filings": 42}`), &rep); err == nil {
		t.Fatal("expected an error for a filings value that is neither an array nor an encoded array")
	}
}
