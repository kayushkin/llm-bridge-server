package main

// Kind is what sort of email a message is, and it decides three things at once:
// what the model is asked to write, whether the message opens a thread or
// continues one, and whether it concerns a work item on the board.
//
// The vocabulary is drawn from what a software team's mailbox actually holds,
// not from what is easy to generate. The classifier's job is telling a task
// ("do this") from a record ("this happened"), and a corpus of nothing but
// clearly-actionable mail would never exercise that.
type Kind string

const (
	KindTaskRequest      Kind = "task_request"
	KindStatusCheck      Kind = "status_check"
	KindCancellation     Kind = "cancellation"
	KindScopeChange      Kind = "scope_change"
	KindReviewResponse   Kind = "review_response"
	KindVendorReply      Kind = "vendor_reply"
	KindIncident         Kind = "incident"
	KindBulkNotification Kind = "bulk_notification"
	KindUnfiled          Kind = "unfiled"
)

// kindSpec is everything that follows from a kind.
type kindSpec struct {
	Kind Kind
	// Continues marks a kind that only ever appears as a reply inside an
	// existing thread. A review response with no pull request to respond to is
	// not a hard case for a classifier, it is nonsense.
	Continues bool
	// NeedsWorkItem marks a kind that must be about a card on the board. The
	// remainder are the noise a real mailbox carries, and the classifier has to
	// survive them.
	NeedsWorkItem bool
	// Senders are the cast keys who plausibly send this. Drawn from, not fixed,
	// so one sender's mail spreads across kinds the way a real colleague's does.
	Senders []string
	// Guidance is handed to the model verbatim. It describes the SHAPE of the
	// message rather than its content, because the content has to come from the
	// work item — a body that ignores the card it is about teaches the
	// classifier nothing.
	Guidance string
}

var kindSpecs = map[Kind]kindSpec{
	KindTaskRequest: {
		Kind: KindTaskRequest, NeedsWorkItem: true,
		Senders:  []string{"priya", "marcus", "helena", "security"},
		Guidance: "Ask the recipient to take on this piece of work. Say why it matters now and what the constraint is. This is the email the work item was created from, so it must read as the FIRST time anyone mentioned it.",
	},
	KindStatusCheck: {
		Kind: KindStatusCheck, NeedsWorkItem: true,
		Senders:  []string{"ops", "priya", "helena"},
		Guidance: "Ask where something stands. Name the specific thing — an order number, a ticket, a release — and say who is waiting on the answer. Do not ask for new work.",
	},
	KindCancellation: {
		Kind: KindCancellation, NeedsWorkItem: true,
		Senders:  []string{"marcus", "priya"},
		Guidance: "Call off work that is already underway, and give the reason it stopped being worth doing. Say plainly what should happen to what has been done so far.",
	},
	KindScopeChange: {
		Kind: KindScopeChange, NeedsWorkItem: true,
		Senders:  []string{"priya", "helena"},
		Guidance: "Add something to work already in flight. Be specific about the addition and about whether the original deadline still stands. This must read as a change to an EXISTING task, not a new one.",
	},
	KindReviewResponse: {
		Kind: KindReviewResponse, Continues: true, NeedsWorkItem: true,
		Senders:  []string{"dinesh", "marcus"},
		Guidance: "Reply to a review request. Be technical and brief. Either approve with a specific observation about the change, or name one concrete thing to fix first.",
	},
	KindVendorReply: {
		Kind: KindVendorReply, Continues: true, NeedsWorkItem: true,
		Senders:  []string{"vendor"},
		Guidance: "Reply as an external vendor's support desk. Formal, a little slow, quote a ticket reference. Either give a partial answer or ask for information already provided.",
	},
	KindIncident: {
		Kind: KindIncident, NeedsWorkItem: true,
		Senders:  []string{"helena", "ops"},
		Guidance: "Report something customer-visible that broke, with a time window and a rough blast radius, and ask for a write-up. Urgent in tone but not panicked.",
	},
	KindBulkNotification: {
		Kind:     KindBulkNotification,
		Senders:  []string{"ci"},
		Guidance: "Write an automated build digest. Terse, machine-generated, numbers and a commit hash. No greeting, no sign-off, no prose. This is deliberately NOT actionable — it is the bulk mail a classifier must learn to file without asking anyone.",
	},
	KindUnfiled: {
		Kind:     KindUnfiled,
		Senders:  []string{"portal"},
		Guidance: "Write a vague automated notification that is genuinely hard to place: it hints at something needing attention without saying what, who it is for, or what to do. This exists to be the message a classifier CANNOT file confidently.",
	},
}

func specFor(kind Kind) kindSpec {
	spec, ok := kindSpecs[kind]
	if !ok {
		panic("synthetic-mail: unknown kind " + string(kind))
	}
	return spec
}
