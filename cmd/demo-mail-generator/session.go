package main

import (
	"fmt"
	"strings"

	"github.com/slava/scheduler/internal/demomail"
)

// sessionPrompt turns the plan into work an agent performs, rather than a value
// it returns.
//
// This is the whole difference between calling the API and running a session.
// An API call could demand a schema and get a validated object back, which the
// caller then delivered. A session has no return value anybody reads, so the
// delivery has to be part of the task: the agent writes the mail AND posts it,
// and the result is visible in the mailbox rather than in a decoded struct.
//
// The trade is deliberate. What is lost is schema validation; what is gained is
// that the work runs on the Max subscription instead of spending API credit,
// and that no OAuth token is handled by anything here.
func sessionPrompt(project *demomail.Project, plan []planned, account, mailURL, mailToken string) string {
	var b strings.Builder

	b.WriteString(`You are writing work email for a software team, into a mailbox used to test
an email classifier. Every message you write must be POSTED, or this task has
produced nothing.

`)
	b.WriteString(project.Brief())

	fmt.Fprintf(&b, `

THE ONE RULE

Every message must be answerable by somebody looking at that repository above.
It is a Go service: do not write about Node, Docker, Kubernetes, a database or
a frontend, because none of them appear in it. Name real packages, real routes,
real files, real commits and real pull request numbers. A request that
contradicts the code wastes a reader's day proving it cannot be done — that has
already happened once here, with a request to bump a Node build image in a
repository that has never contained a line of JavaScript.

HOW TO WRITE

Write like a colleague, not like a ticket. Real mail is uneven: some messages
are three lines, some ramble, some open with context nobody needed. Do not make
them uniform and do not write subject lines that read like Jira summaries. Do
not invent dates or people. Do not sign off as anyone but the named sender.

WRITE AND POST %d MESSAGE(S)

For each one below, compose it and then POST it. Post each message as you
finish it rather than saving them all for the end, so a failure part way
through still leaves the earlier ones delivered.

`, len(plan))

	for _, m := range plan {
		fmt.Fprintf(&b, "--- %s\n", m.Key)
		fmt.Fprintf(&b, "from: %s <%s> — %s\n", m.From.Name, m.From.Email, m.From.Voice)
		fmt.Fprintf(&b, "kind: %s — %s\n\n", m.Kind.Name, m.Kind.Wants)
	}

	fmt.Fprintf(&b, `POST each one like this, substituting the sender, subject and body:

  curl -s -X POST '%s/api/accounts/%s/messages' \
    -H 'Authorization: Bearer %s' \
    -H 'Content-Type: application/json' \
    -d '{"subject":"…","from":{"name":"…","email":"…"},
         "to":[{"name":"Vlad","email":"vlad@northwind-eng.example"}],
         "body_text":"…"}'

A 201 with a "locator" field means it was delivered. Anything else is a
failure worth reporting rather than retrying blindly.

Use a heredoc or a file for the JSON if a body contains quotes or newlines —
a body mangled by shell quoting is worse than one that was never sent, because
it looks delivered.

When you are done, say which messages you posted and their locators. Do not
post more than the %d asked for.
`, mailURL, account, mailToken, len(plan))

	return b.String()
}
