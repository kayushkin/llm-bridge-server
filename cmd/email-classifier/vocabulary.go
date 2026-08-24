package main

import "fmt"

// actionNone is the "no action, ever" value of the action axis, and it is shared
// by every vocabulary. Named because the sender override in noaction_senders.go
// writes it, and a second spelling of it would be a vocabulary violation nothing
// would catch.
const actionNone = "none"

// A vocabulary is the whole classification scheme for one KIND of mailbox: what
// topics exist, what you might have to do, what that is worth, and which of it
// lands in front of you.
//
// There is more than one because a personal mailbox and a work mailbox are not
// the same problem wearing different words. "pay" is the severe case in a
// personal inbox and does not occur in a work one; "review" is the thing that
// blocks a colleague at work and means nothing at home. Classifying work mail
// against the personal vocabulary forced everything real into "work-career" and
// "fix", which is one bucket and one verb for the entire job.
//
// Selected once at start-up and never per-message: a mailbox is one kind of
// mailbox, and letting the model pick a scheme per email would make the axes
// incomparable across the board they all land on.
type vocabulary struct {
	// Name is what -vocabulary takes.
	Name string
	// Domain is one line telling the model what sort of mailbox this is. It
	// leads the prompt, because every axis below reads differently depending on
	// the answer.
	Domain string

	Categories []string
	Actions    []string
	Urgencies  []string

	// ActionsNeedingWork are the actions that put a card in the Action needed
	// column. Everything else lands in No action.
	ActionsNeedingWork map[string]bool

	// ActionStakes and CategoryStakes score consequence on each axis. An action
	// scoring stakesNone GATES the card to zero however weighty its category —
	// see stakesOf.
	ActionStakes   map[string]stakes
	CategoryStakes map[string]stakes

	// MootCategoriesWhenPassed and MootActionsWhenPassed are the things whose
	// deadlines stop mattering once they pass, as against the ones where being
	// late keeps costing something.
	MootCategoriesWhenPassed map[string]bool
	MootActionsWhenPassed    map[string]bool

	// Guidance is appended to the system prompt: the judgement calls that are
	// specific to this kind of mail and that the axis lists alone do not convey.
	Guidance string
}

// personalVocabulary is the original scheme, unchanged. It classifies the
// author's own inbox, and the live scheduler job depends on every value here
// staying exactly as it was.
var personalVocabulary = vocabulary{
	Name:   "personal",
	Domain: "a person's own email: bills, appointments, orders, subscriptions, and mail from actual humans.",

	Categories: []string{
		"finance",          // bills, payments, receipts, banking (Venmo, PayPal, Link)
		"housing",          // rent, landlord, apartment hunting (AppFolio, Apartment List)
		"health",           // appointments, insurance, providers (Tebra, Anthem)
		"commerce",         // orders, shipping, returns, marketplace (eBay, Etsy, USPS)
		"work-career",      // jobs, recruiters, professional (Glassdoor, Indeed)
		"dev-infra",        // repos, domains, model providers, service alerts
		"social-events",    // meetups, classes, community (Meetup, dance, ballet)
		"government-legal", // DMV, USPS official, taxes, governing bodies
		"media-subs",       // digests and subscriptions (Medium, MyAnimeList, Plex)
		"politics",         // campaign and advocacy mail
		"promotions",       // pure marketing
		"personal",         // mail from an actual human
	},
	Actions: []string{
		"reply",    // a counterparty is waiting on a response
		"pay",      // money is due
		"decide",   // accept/decline/choose, usually against a deadline
		"schedule", // book, attend, or move an appointment
		"verify",   // confirm identity or a security event
		"fix",      // something is broken and needs work
		"read",     // worth reading, nothing owed
		"file",     // receipt or record, keep for reference
		actionNone, // no action, ever
	},
	Urgencies: sharedUrgencies,

	ActionsNeedingWork: map[string]bool{
		"reply": true, "pay": true, "decide": true,
		"schedule": true, "verify": true, "fix": true,
	},
	ActionStakes: map[string]stakes{
		"pay":      stakesSevere,
		"verify":   stakesSevere,
		"fix":      stakesReal,
		"decide":   stakesReal,
		"reply":    stakesReal,
		"schedule": stakesReal,
		"read":     stakesNone,
		"file":     stakesNone,
		"none":     stakesNone,
	},
	CategoryStakes: map[string]stakes{
		"finance":          stakesSevere,
		"housing":          stakesSevere,
		"health":           stakesSevere,
		"government-legal": stakesSevere,
		"dev-infra":        stakesReal,
		"work-career":      stakesReal,
		"commerce":         stakesReal,
		"personal":         stakesReal,
		"social-events":    stakesMinor,
		"media-subs":       stakesMinor,
		"politics":         stakesNone,
		"promotions":       stakesNone,
	},
	MootCategoriesWhenPassed: map[string]bool{
		"social-events": true,
		"media-subs":    true,
		"promotions":    true,
		"politics":      true,
	},
	MootActionsWhenPassed: map[string]bool{
		"verify": true,
	},
	// These moved here from the shared prompt, where they had always been. They
	// name brands and describe one particular inbox — telling a work classifier
	// about Zillow open houses and Amazon receipts spends prompt on cases it will
	// never see, and invites it to read a build digest as junk mail for the wrong
	// reason.
	Guidance: `A RECEIPT IS NOT A TASK. Payment confirmations, order receipts, statements and delivery notices are records: nothing is owed and nothing will ever be completed. File them on a durable bucket card for that source or topic — "Payment receipts", "Rent payments", "Amazon orders" — never on a card of their own. A board with one card per receipt is a board nobody can read. The same holds for one-off newsletters and announcements: they belong on a bucket for that sender.

Listings feeds are never actionable. Mail from a property or marketplace listings feed — Zillow open houses, price cuts, saved-search hits — is action "none", however specific a date it names. An open house on Saturday is not something owed to anyone.

Judge the email, not its vocabulary. Marketing mail routinely says "REMINDER", "Action required", "expiring", "last chance" — that is copywriting, not a task. A real task has a specific counterparty expecting something specific from this user.`,
}

// workVocabulary classifies a software engineer's work mail.
//
// The axes are chosen around the question a work inbox actually poses, which is
// not "what is this about" but "is somebody blocked on me, and what does it cost
// if I leave it until tomorrow".
//
// Three deliberate departures from the personal scheme:
//
//   - "review" is its own action and scores as real. A pull request waiting on
//     you blocks another person's day, which almost nothing in a personal inbox
//     does.
//   - "automation" gates to zero the way "promotions" does. Build digests and
//     monitoring chatter are the bulk of a work inbox by volume and almost none
//     of it by consequence, and a nightly digest counting down to a deadline is
//     still a nightly digest.
//   - "incident" and "customer" are severe, because the cost of being late is
//     paid by somebody outside the team. "release" is only real: shipping late
//     is expensive, but it is a cost the team absorbs itself.
var workVocabulary = vocabulary{
	Name:   "work",
	Domain: "the work email of a software engineer on a product team: requests for changes, code review, incidents, releases, and a great deal of automated noise.",

	Categories: []string{
		"incident",       // something is broken for users right now
		"customer",       // a named customer is affected or waiting
		"security",       // vulnerabilities, credential rotation, audits
		"release",        // shipping, deploys, release coordination
		"code-review",    // pull requests and change proposals
		"product",        // requirements, scope, roadmap, prioritisation
		"infrastructure", // build, CI, environments, platform
		"vendor",         // third parties and their support desks
		"team",           // process, planning, scheduling, people
		"admin",          // expenses, compliance, HR, training
		"automation",     // build digests, monitoring, bots
	},
	Actions: []string{
		"implement",   // write or change code
		"review",      // review somebody else's change; they are blocked until you do
		"investigate", // find out what is happening before anything can be decided
		"respond",     // answer a question; no code changes hands
		"deploy",      // ship, roll back, or run a release step
		"decide",      // choose between options, usually so others can proceed
		"escalate",    // this is not yours to solve and somebody must be told now
		"document",    // write it down: a review, a runbook, a decision record
		"read",        // worth knowing, nothing owed
		actionNone,    // no action, ever
	},
	Urgencies: sharedUrgencies,

	// Everything except read and none. Work mail that names an action is work,
	// even when the action is only writing something down — a postmortem nobody
	// files is a postmortem nobody has.
	ActionsNeedingWork: map[string]bool{
		"implement": true, "review": true, "investigate": true,
		"respond": true, "deploy": true, "decide": true,
		"escalate": true, "document": true,
	},
	ActionStakes: map[string]stakes{
		"escalate":    stakesSevere,
		"deploy":      stakesReal,
		"implement":   stakesReal,
		"review":      stakesReal,
		"investigate": stakesReal,
		"respond":     stakesReal,
		"decide":      stakesReal,
		"document":    stakesMinor,
		"read":        stakesNone,
		"none":        stakesNone,
	},
	CategoryStakes: map[string]stakes{
		"incident":       stakesSevere,
		"customer":       stakesSevere,
		"security":       stakesSevere,
		"release":        stakesReal,
		"code-review":    stakesReal,
		"product":        stakesReal,
		"infrastructure": stakesReal,
		"vendor":         stakesReal,
		"team":           stakesMinor,
		"admin":          stakesMinor,
		"automation":     stakesNone,
	},
	// A build that failed last night and has since gone green is history; a
	// planning meeting that happened is over. An incident whose deadline passed
	// is emphatically NOT moot, and neither is a security rotation — both get
	// worse by being late, which is why neither appears here.
	MootCategoriesWhenPassed: map[string]bool{
		"automation": true,
		"team":       true,
		"admin":      true,
	},
	// A review requested for a change that has already shipped cannot be given
	// retroactively, and a deploy window that closed has closed. Implementation
	// is the deliberate contrast and must never be added: work asked for and not
	// done is exactly the thing that keeps costing.
	MootActionsWhenPassed: map[string]bool{
		"review": true,
		"deploy": true,
	},
	Guidance: `
This is work mail. Judge every message by whether somebody is blocked on the
recipient, not by how loudly it is written.

  - A pull request awaiting review blocks a colleague's day. It is action:review
    and it is not low urgency, even when nobody says so.
  - Automated build and monitoring mail is cat:automation and action:none, even
    when it says FAILED. A red build is a fact about a build; a person asking
    you to fix a red build is action:implement.
  - A question with no change attached is action:respond, not action:implement.
    Answering is the whole of the work, and filing it as implementation makes it
    look unfinished forever.
  - A request that changes work already underway belongs on the EXISTING card
    for that work, not on a new one. Scope growing is not a second task.
  - An outage or a named customer waiting is cat:incident or cat:customer, and
    those are severe wherever they appear — the cost of being slow is paid by
    somebody outside the team.`,
}

// sharedUrgencies is the time axis, and it is deliberately the same in both
// schemes. Time is time: how long is left does not depend on whether the mail
// is about rent or about a release. "expired" earns its place in both, because
// a large share of what reads as urgent is a deadline already missed, and
// collapsing that into "low" hides the difference between "ignore this" and
// "you missed it".
var sharedUrgencies = []string{"critical", "high", "normal", "low", "expired"}

var vocabularies = map[string]*vocabulary{
	personalVocabulary.Name: &personalVocabulary,
	workVocabulary.Name:     &workVocabulary,
}

// active is the vocabulary this run classifies against.
//
// It defaults to personal so that the live job, which passes no flag, keeps
// behaving exactly as it did.
var active = &personalVocabulary

// selectVocabulary switches schemes, refusing a name it does not know rather
// than falling back. A typo silently classifying work mail against the personal
// axes would produce a board that looks fine and means nothing.
func selectVocabulary(name string) error {
	v, ok := vocabularies[name]
	if !ok {
		return fmt.Errorf("unknown vocabulary %q; known: personal, work", name)
	}
	active = v
	return nil
}
