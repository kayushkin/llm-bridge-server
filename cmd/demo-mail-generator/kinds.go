package main

import "github.com/slava/scheduler/internal/demoteam"

// requestKind is what somebody wants, and the mix matters more than any single
// message. A mailbox of nothing but "please build this" tests one path; real
// work email is mostly other things.
type requestKind struct {
	Name string
	// From is the role that plausibly sends it.
	From demoteam.Role
	// Wants describes the shape of the ask to the model.
	Wants string
}

var requestKinds = []requestKind{
	{
		Name: "change", From: demoteam.RoleResearcher,
		Wants: "Ask for a specific, small change to the code. Name the actual file, package or route it touches. It must be something this repository could really accept — not a technology it does not use.",
	},
	{
		Name: "question", From: demoteam.RoleResearcher,
		Wants: "Ask a question about how something in the code works, or what a behaviour is. Want an ANSWER, not a change. Do not ask for anything to be built.",
	},
	{
		Name: "scope-change", From: demoteam.RoleResearcher,
		Wants: "Add something to work that is already in flight — reference one of the OPEN pull requests by number and say what else it now needs. This is a change to existing work, not a new task.",
	},
	{
		Name: "review-nudge", From: demoteam.RoleReviewer,
		Wants: "Chase a review on one of the OPEN pull requests, naming it by number. Say what you want looked at specifically.",
	},
	{
		Name: "reconsider", From: demoteam.RoleResearcher,
		Wants: "Question whether some work already done or in progress was the right call, and ask for it to be revisited or backed out. Reference something real from the commit history or an open pull request.",
	},
	{
		Name: "bug-report", From: demoteam.RoleResearcher,
		Wants: "Report something behaving wrong, with the route or function it happens in and what you expected instead. Base it on behaviour the code actually has.",
	},
}
