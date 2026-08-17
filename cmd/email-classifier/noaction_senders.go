package main

import "strings"

// ==================== senders that are never actionable ====================
//
// Some senders only ever produce feeds. No message from them is owed to anyone,
// however urgent an individual one reads, so the action axis is decided by the
// sender rather than by the model's reading of the text.
//
// This is a deliberate override and not a hint in the prompt, because the model
// was not wrong on the message it saw — it was wrong about the feed. Told that
// an open house is on Saturday, reading that as a decision against a deadline is
// the correct reading of that sentence. The rule that fixes it has to live
// outside the sentence.

// alwaysNoActionDomains is the list. Kept as registrable domains, matched
// against the sender's host and any subdomain of it, because this kind of mail
// arrives from a different subdomain almost every time — Zillow alone sends from
// mail.zillow.com and zmail.zillow.com.
var alwaysNoActionDomains = []string{
	// Zillow is a listings feed: open houses, price cuts, saved-search hits.
	// One message filed as action:decide made the card severe stakes via
	// cat:housing, and once its date passed a two-week-old open house sat at
	// the top of the board on the maximum score.
	"zillow.com",
}

// senderIsAlwaysNoAction reports whether mail from this address can ever need
// action. The address is expected in the form senderKey produces.
func senderIsAlwaysNoAction(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(email[at+1:]))
	for _, domain := range alwaysNoActionDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// anySenderIsAlwaysNoAction reports whether a card's learned senders include one
// whose mail is never actionable. Used to stop a later message raising a card
// the sender rule already settled.
func anySenderIsAlwaysNoAction(senders []string) bool {
	for _, s := range senders {
		if senderIsAlwaysNoAction(s) {
			return true
		}
	}
	return false
}
