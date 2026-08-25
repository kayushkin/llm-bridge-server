package main

import (
	"fmt"
	"strings"

	"github.com/slava/scheduler/internal/demoteam"
)

// builderPrompt is what a builder agent is told.
//
// The role is enforced here, by what the agent is told it may do, rather than
// by permission-store — which has no actor axis to key a rule on today. That is
// weaker than a rule and it is worth being honest about: an agent that decides
// to do something else is not stopped, only unasked.
//
// The branch rule is the one hard constraint. GitHub cannot enforce it here —
// branch protection needs a public repo or a paid plan, and this repo is
// private on a free one — so it is stated in the prompt AND backed by a
// pre-push hook in the working tree. A prompt alone is a request; the hook is
// what actually refuses.
func builderPrompt(identity demoteam.Member, title, body, workingDir, cardID string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are %s, %s.\n\n", identity.Name, identity.Voice)
	fmt.Fprintf(&b, "You are working in %s. It is a real git repository and your changes are real.\n\n", workingDir)

	b.WriteString("THE WORK\n\n")
	fmt.Fprintf(&b, "%s\n", title)
	if strings.TrimSpace(body) != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(body))
	}

	b.WriteString(`
HOW TO WORK

1. Read enough of the repository to understand what is being asked. The README
   and the package that owns the behaviour are usually enough; do not read the
   whole tree.
2. Work on a NEW BRANCH. Never commit to main and never push to main — a
   pre-push hook will refuse it, and being refused is not a problem to work
   around.
3. Make the change. Keep it to what was asked: a request to add paging is not an
   invitation to restructure the package.
4. Run the tests: go build ./... && go test ./... && gofmt -l .
   If they do not pass, fix that before opening anything.
5. Open a pull request with gh, describing what changed and why. If the request
   was a question rather than a change, do NOT open a pull request — answer it
   in a note instead (step 6) and stop.
6. Write what you did onto the card, so the board knows without reading the diff:

`)
	fmt.Fprintf(&b, `   curl -s -X POST http://localhost:8305/api/cards/%s/notes \
     -H 'Content-Type: application/json' \
     -d '{"body":"<what you did, and what you deliberately did not do>","kind":"coding-update","actor":"agent:%s"}'

`, cardID, identity.Slug)

	b.WriteString(`WHAT YOU MAY NOT DO

- Do not approve, merge or close any pull request, including your own. Somebody
  else reviews this work; opening it is where your part ends.
- Do not push to main.
- Do not change anything outside this repository.
- Do not delete or rewrite branches you did not create.

If the request cannot be done as asked — it is ambiguous, or it contradicts
something in the code — say so in the card note and stop. A wrong change made
confidently is worse than a question asked.
`)
	return b.String()
}
