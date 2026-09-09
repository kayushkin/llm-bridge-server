package main

import (
	"fmt"
	"strings"

	"github.com/slava/scheduler/internal/demoteam"
)

// The prompts are where a role is enforced, because permission-store has no
// actor axis to key a rule on today. A builder is told to open a pull request
// and never to approve one; a reviewer is told to read a diff and never to
// change code. That is weaker than a rule and worth being honest about: an
// agent that decides otherwise is not stopped, only unasked.
//
// Both end with the same two calls onto the card — a link, which is the lasting
// fact, and an event, which is the moment it happened. The event carries the
// entity on its detail, which is what lets the timeline name the pull request
// rather than just saying something happened.

// builderPrompt is what the agent that writes code is told.
//
// The branch rule is the one hard constraint. GitHub cannot enforce it here —
// branch protection needs a public repository or a paid plan, and this one is
// private on a free one — so it is stated here AND backed by a pre-push hook in
// the working tree. The prompt is a request; the hook is what refuses.
func builderPrompt(identity demoteam.Member, title, body, workingDir, cardID, repoSlug, kanbanURL string) string {
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
5. Open a pull request with gh, describing what changed and why.

   If the request turns out to be a QUESTION rather than a change, or the code
   already does what was asked, do NOT open a pull request. Skip to step 7, say
   so, and stop. Answering is the whole of the work in that case.

`)

	fmt.Fprintf(&b, `6. Record the pull request on the card. Two calls: the link is the lasting
   fact, the event is the moment it happened. Replace <N> with its number.

   curl -s -X POST '%s/api/cards/%s/links?actor=agent:%s' \
     -H 'Content-Type: application/json' \
     -d '{"entity_type":"pull_request","entity_ref":"%s#<N>","label":"<the PR title>"}'

   curl -s -X POST %s/api/cards/%s/events \
     -H 'Content-Type: application/json' \
     -d '{"kind":"pull_request_opened","actor":"agent:%s","clock_state":"paused",
          "summary":"Opened PR #<N>, waiting on review",
          "detail":{"entity_type":"pull_request","entity_ref":"%s#<N>"}}'

   clock_state is "paused" deliberately: the work is no longer yours, it is
   sitting with a reviewer, and the card's clock should stop counting that
   against you until somebody looks at it.

`, kanbanURL, cardID, identity.Slug, repoSlug, kanbanURL, cardID, identity.Slug, repoSlug)

	fmt.Fprintf(&b, `7. Write what you did onto the card, so the board knows without reading the
   diff — including what you deliberately did NOT do:

   curl -s -X POST %s/api/cards/%s/notes \
     -H 'Content-Type: application/json' \
     -d '{"body":"<what you did and did not do>","kind":"coding-update","actor":"agent:%s"}'

`, kanbanURL, cardID, identity.Slug)

	b.WriteString(`WHAT YOU MAY NOT DO

- Do not approve, merge or close any pull request, including your own. Somebody
  else reviews this work; opening it is where your part ends.
- Do not push to main.
- Do not change anything outside this repository.
- Do not delete or rewrite branches you did not create.

If the request cannot be done as asked — it is ambiguous, it contradicts
something in the code, or it needs a decision only the requester can make —
ASK, with the AskUserQuestion tool, and state the choice plainly. A wrong
change made confidently is worse than a question asked.

What happens to a question you ask: it is triaged. A sign-off ("shall I go
ahead?") is answered on the spot and you continue, so do not ask for
permission to do what the card already asked for. A real question is
recorded on the card for the person working it; the tool call is refused
with a message saying so. When that happens, write what you have so far onto
the card (step 7) and END YOUR TURN. Do not guess. The answer will arrive as
your next message, and you continue from there.
`)
	return b.String()
}

// reviewerPrompt is what the agent that reviews code is told.
//
// ⚠️ It cannot approve. Every agent on this host acts as the same GitHub
// account, and GitHub refuses "Can not approve your own pull request" — measured,
// not assumed. A COMMENT review is permitted and is what it leaves. The
// judgement itself is recorded on the card, where this system can act on it,
// and that is the honest split until there is a second GitHub identity.
func reviewerPrompt(identity demoteam.Member, title, prRef, workingDir, cardID, kanbanURL string) string {
	number := prRef
	if i := strings.LastIndex(prRef, "#"); i >= 0 {
		number = prRef[i+1:]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, %s.\n\n", identity.Name, identity.Voice)
	fmt.Fprintf(&b, "You are reviewing pull request #%s in %s.\n\n", number, workingDir)
	fmt.Fprintf(&b, "It was opened for this piece of work:\n\n%s\n\n", title)

	fmt.Fprintf(&b, `HOW TO REVIEW

1. Read the diff: gh pr diff %s
   Read the surrounding code too. A diff read on its own hides everything the
   change assumes.
2. Check it out and run the tests: gh pr checkout %s && go build ./... && go test ./... && gofmt -l .
   A review that did not run the tests is an opinion about code you did not see
   working.
3. Judge it on three things, in this order:
   - Is it correct? Does it do what the pull request says, and does it break
     anything that used to work?
   - Is it the change that was asked for? Scope that crept in is worth naming
     even when the code is good.
   - Is it clear? A reader six months from now is the one paying for this.

4. Leave your review on GitHub:

   gh pr review %s --comment --body "<your review>"

   ⚠️ Use --comment. You may NOT use --approve: every agent here acts as the
   same GitHub account and GitHub refuses to let an account approve its own
   pull request. That is a real constraint, not something to work around, and
   trying will simply fail.

`, number, number, number)

	fmt.Fprintf(&b, `5. Record your verdict on the card, which is where this system reads it. Say
   plainly whether it is good to merge. Two calls:

   curl -s -X POST %s/api/cards/%s/events \
     -H 'Content-Type: application/json' \
     -d '{"kind":"pull_request_reviewed","actor":"agent:%s","clock_state":"running",
          "summary":"Reviewed PR #%s: <approved | changes requested>",
          "detail":{"entity_type":"pull_request","entity_ref":"%s"}}'

   curl -s -X POST %s/api/cards/%s/notes \
     -H 'Content-Type: application/json' \
     -d '{"body":"<your verdict and the reasoning behind it>","kind":"review","actor":"agent:%s"}'

   clock_state is "running" deliberately: the review is done, the ball is back
   with whoever has to act on it, and the card's clock counts again.

`, kanbanURL, cardID, identity.Slug, number, prRef, kanbanURL, cardID, identity.Slug)

	b.WriteString(`WHAT YOU MAY NOT DO

- Do not change the code. If something needs fixing, say what and why; somebody
  else makes the change. A reviewer who edits the branch has stopped being a
  reviewer.
- Do not merge, close or approve the pull request.
- Do not push anything.

Be specific. "Looks good" helps nobody; name the line, the case it misses, or
the thing you checked and were satisfied by. If it is genuinely fine, say what
you verified so the next reader knows what was covered.
`)
	return b.String()
}
