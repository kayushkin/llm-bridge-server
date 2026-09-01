package main

import (
	"fmt"
	"strings"

	"github.com/slava/scheduler/internal/demomail"
	"github.com/slava/scheduler/internal/demoteam"
)

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

// systemPrompt is the standing instruction for composing the mail.
//
// The one rule at the top is the one that matters. The first version of this
// demo wrote requests without reading the repository, and one asked an agent to
// bump a Node build image in a Go service that has never contained a line of
// JavaScript. The agent checked every branch and all twelve commits, refused,
// and wrote down why — behaving exactly as instructed, at the cost of a real
// session.
const systemPrompt = `You write work email for a software team, about a REAL repository whose
contents you are given.

The single rule that matters: every message must be answerable by somebody
looking at that repository. If the code is a Go service, do not write about
Node, Docker, Kubernetes, a database or a frontend unless the files you were
shown contain them. Name real packages, real routes, real files, real commits
and real pull request numbers. A request that contradicts the code wastes the
reader's day proving it cannot be done.

Write like a colleague, not like a ticket. Real mail is uneven: some messages
are three lines, some ramble, some open with context nobody needed. Do not make
them uniform, and do not write a subject line that reads like a Jira summary.

Do not invent dates, deadlines or people. Refer to timing in relative terms
("before the release", "when you get a moment") unless the material you were
given contains a date.

Do not sign off as anyone other than the named sender, and never write the
recipient's reply.`

// writeSchema forces the shape of the answer. Every field is required: a
// message missing its subject or body is not a partial success, it is a gap
// that would only be found later by reading the mailbox.
func writeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"messages": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"key":     map[string]any{"type": "string", "description": "The key of the message being written, copied exactly."},
						"subject": map[string]any{"type": "string", "description": "The Subject header."},
						"body":    map[string]any{"type": "string", "description": "The plain-text body, with real newlines."},
					},
					"required":             []string{"key", "subject", "body"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"messages"},
		"additionalProperties": false,
	}
}

// buildBrief hands the model the project and the plan.
func buildBrief(project *demomail.Project, plan []planned) string {
	var b strings.Builder
	b.WriteString(project.Brief())
	fmt.Fprintf(&b, "\n\nWrite %d message(s).\n\n", len(plan))
	for _, m := range plan {
		fmt.Fprintf(&b, "--- key: %s\n", m.Key)
		fmt.Fprintf(&b, "kind: %s\n", m.Kind.Name)
		fmt.Fprintf(&b, "from: %s <%s> — %s\n", m.From.Name, m.From.Email, m.From.Voice)
		fmt.Fprintf(&b, "what they want: %s\n\n", m.Kind.Wants)
	}
	return b.String()
}
