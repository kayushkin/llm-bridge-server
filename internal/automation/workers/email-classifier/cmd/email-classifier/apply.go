package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// applier turns filing decisions into kanban writes. It is the only place that
// writes, so the every-email-is-filed invariant has exactly one enforcement
// point: linkEmail refuses an empty card list, and applyReport routes anything
// the model skipped to the triage card before it can reach that refusal.
type applier struct {
	cfg   config
	kc    *issuesclient.Client
	state *boardState

	// gate keeps the classifier off cards an agent is currently working on.
	// See session.go for which states count as working.
	gate *sessionGate

	// contacts resolves a sender's address to the contact a ticket names as
	// its requester. Nil leaves cards as plain cards; see ticket.go.
	contacts contactResolver
	// putTicket writes the ticket; nil means the kanban client's own call.
	putTicket ticketWriter

	filed          int
	cardsCreated   int
	cardsUpdated   int
	ticketsWritten int
	ticketsFailed  int

	// skippedAgentOwned counts cards left alone this tick because an agent held
	// them. Reported at the end so a board that stops changing has a visible
	// reason rather than looking like the classifier silently died.
	skippedAgentOwned int

	// touched is the set of cards whose evidence list changed this tick and so
	// needs its body rewritten once, at the end, rather than once per link.
	touched map[string]bool

	triageCardID string
}

func (a *applier) markTouched(cardID string) {
	if a.touched == nil {
		a.touched = map[string]bool{}
	}
	a.touched[cardID] = true
}

// ============================ linking ============================

// linkEmail attaches one message to every card given. This is the invariant's
// enforcement point: an empty card list is a programming error, not a tolerable
// outcome, because it would drop the email silently and the next tick would see
// it as unfiled forever.
func (a *applier) linkEmail(cardIDs []string, em pendingEmail, label string) error {
	if len(cardIDs) == 0 {
		return fmt.Errorf("refusing to file %s onto zero cards: every email must reach at least one card", em.Locator)
	}
	for _, cardID := range cardIDs {
		card, ok := a.state.Cards[cardID]
		if !ok {
			log.Printf("[email] WARNING: filing %s onto unknown card %s; skipping that card", em.Locator, cardID)
			continue
		}
		if a.cfg.DryRun {
			log.Printf("[email] DRY-RUN link %s -> card %q (%s)", em.Locator, card.Title, label)
			continue
		}

		if err := a.kc.AddCardLink(cardID, linkTypeEmail, em.Locator, label); err != nil {
			return fmt.Errorf("link %s to card %s: %w", em.Locator, cardID, err)
		}
		card.EmailLinks = append(card.EmailLinks, issuesclient.CardLink{
			CardID: cardID, EntityType: linkTypeEmail, EntityRef: em.Locator, Label: label,
		})
		a.state.FiledLocators[em.Locator] = true
		a.markTouched(cardID)

		// The RFC 5322 identity is stored as its own link so a copy of this
		// message arriving in another account, or a reply quoting it, resolves
		// to this card without a model call. Absent only when the provider fetch
		// failed and the message was filed on its headers alone.
		if em.MessageID != "" && !containsString(a.state.CardsByMessageID[em.MessageID], cardID) {
			if err := a.kc.AddCardLink(cardID, linkTypeEmailMessageID, em.MessageID, ""); err != nil {
				// Losing the durable identity costs future dedup, not this
				// filing, so it is a warning rather than a failure.
				log.Printf("[email] WARNING: message-id link for %s: %v", em.Locator, err)
			} else {
				a.state.CardsByMessageID[em.MessageID] = append(a.state.CardsByMessageID[em.MessageID], cardID)
			}
		}

		// Record which addresses feed this card. This is evidence, not a routing
		// rule: it tells the merge pass which cards draw on the same sources and
		// it answers the never-actionable domain override. No mail is filed onto
		// a card because an earlier message from that address landed there —
		// that rule existed here, and one address is not one topic.
		//
		// Kept to buckets because that is where the merge pass looks. A task
		// card's sender says nothing about the next task.
		if card.ColumnName == columnNoAction {
			sender := senderKey(em.Meta.From)
			if sender != "" && !containsString(a.state.CardsBySender[sender], cardID) {
				if err := a.kc.AddCardLink(cardID, linkTypeEmailSender, sender, card.Title); err != nil {
					log.Printf("[email] WARNING: sender link %s: %v", sender, err)
				} else {
					a.state.CardsBySender[sender] = append(a.state.CardsBySender[sender], cardID)
				}
			}
		}
	}
	a.filed++
	return nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// applyDirectRoutes files the emails the cheap paths already placed.
func (a *applier) applyDirectRoutes(routes []directRoute) error {
	for _, r := range routes {
		if err := a.linkEmail(r.CardIDs, r.Email, r.Label); err != nil {
			log.Printf("[email] direct route (%s) failed for %s: %v", r.Reason, r.Email.Locator, err)
			continue
		}
		log.Printf("[email] filed %s via %s", r.Email.Locator, r.Reason)
	}
	return nil
}

// ============================ report application ============================

// applyReport creates the cards the model asked for, files every email in the
// batch, and applies state changes to cards the new mail moved.
func (a *applier) applyReport(batch []pendingEmail, report *filingReport) error {
	// Titles of every card already on the board, so a card the model proposes
	// that already exists is reused rather than duplicated. Duplicating a card
	// splits one thread of work across two places, which is the most expensive
	// mistake available here. The same index absorbs cards created during this
	// batch, which is what makes two filings proposing the same new card
	// converge on one.
	cardIDByTitle := map[string]string{}
	for id, c := range a.state.Cards {
		cardIDByTitle[normalizeTitle(c.Title)] = id
	}

	filingByRef := map[string]emailFiling{}
	for _, f := range report.Filings {
		filingByRef[f.EmailRef] = f
	}

	for _, em := range batch {
		filing, ok := filingByRef[em.Locator]
		if !ok {
			// The model returned no filing for this email at all. That is a
			// violation of its instructions, and dropping the email here is
			// exactly what the triage card exists to prevent.
			log.Printf("[email] WARNING: model returned no filing for %s (%q); sending to triage",
				em.Locator, em.Meta.Subject)
			if err := a.fileToTriage(em); err != nil {
				log.Printf("[email] triage %s: %v", em.Locator, err)
			}
			continue
		}

		cardIDs := a.resolveFilingCards(filing, cardIDByTitle, em.Locator, senderKey(em.Meta.From), em.Meta.From.Name)
		if len(cardIDs) == 0 {
			log.Printf("[email] WARNING: filing for %s (%q) named no resolvable card; sending to triage",
				em.Locator, em.Meta.Subject)
			if err := a.fileToTriage(em); err != nil {
				log.Printf("[email] triage %s: %v", em.Locator, err)
			}
			continue
		}
		if err := a.linkEmail(cardIDs, em, emailLinkLabel(em.Meta, filing.LinkLabel)); err != nil {
			log.Printf("[email] file %s: %v", em.Locator, err)
		}
	}

	for _, upd := range report.CardUpdates {
		if err := a.applyCardUpdate(upd); err != nil {
			log.Printf("[email] update card %s: %v", upd.CardID, err)
		}
	}
	return nil
}

// resolveFilingCards turns a filing's card references into card ids, creating the
// inline new card if it names one. Both kinds are honored together — that is the
// many-to-many case, an email that belongs on an existing card and a new one.
//
// cardIDByTitle is mutated as cards are created, so a later filing in the same
// batch proposing the same title reuses the card rather than making a second one.
// resolveFilingCards turns one message's filing into card ids, creating the
// card it names when that card does not exist yet.
//
// emailLocator is the message's mailstack locator, recorded as the source of
// any ticket this filing makes. senderAddress is senderKey's output and decides
// the feed override below; senderName is what the mail called them, and is
// used only when a card becomes a ticket and the sender has never been seen
// before.
func (a *applier) resolveFilingCards(filing emailFiling, cardIDByTitle map[string]string, emailLocator, senderAddress, senderName string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		if _, known := a.state.Cards[id]; !known {
			log.Printf("[email] WARNING: filing for %s names card id %s that is not on this board; ignoring",
				filing.EmailRef, id)
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range filing.CardIDs {
		add(id)
		// Mail landing on a card that has no requester yet makes it a ticket:
		// this is the message somebody wrote, and the card is what came of it.
		// A card that already has one keeps it.
		if card := a.cardState(id); card != nil {
			a.writeTicket(id, card.ColumnName, emailLocator, senderAddress, senderName)
		}
	}
	if spec := filing.NewCard; spec != nil && strings.TrimSpace(spec.Title) != "" {
		// The sender decides the action axis for feeds, overriding whatever the
		// model made of this one message. Logged rather than applied silently:
		// the override is a claim about the sender, and a wrong entry in that
		// list would otherwise mute a real task without trace.
		if senderIsAlwaysNoAction(senderAddress) && spec.Action != actionNone {
			log.Printf("[email] %s only sends feeds; filing %q as action=%s instead of %q",
				senderAddress, spec.Title, actionNone, spec.Action)
			spec.Action = actionNone
		}
		key := normalizeTitle(spec.Title)
		if existing, ok := cardIDByTitle[key]; ok {
			add(existing)
		} else if id, err := a.createCard(*spec); err != nil {
			log.Printf("[email] create card %q: %v", spec.Title, err)
		} else {
			cardIDByTitle[key] = id
			// The card was made for this message, so this sender is the one
			// who asked. A card in the no-action bucket is skipped inside.
			if card := a.cardState(id); card != nil {
				a.writeTicket(id, card.ColumnName, emailLocator, senderAddress, senderName)
			}
			add(id)
		}
	}
	return out
}

// holdReason is what a parked card says about itself. Written out rather than
// left blank so that a human meeting one on the board is told who is expected
// to release it, not merely that it is stuck.
const holdReason = "filed by email-classifier for a pipeline that has not picked it up yet"

func (a *applier) createCard(spec newCardSpec) (string, error) {
	// Report vocabulary violations rather than coercing them. A tag carrying an
	// unexpected value is visible in the UI's filter list and can be corrected;
	// a silently rewritten one is a wrong answer that looks right.
	for _, axis := range []struct {
		name   string
		value  string
		values []string
	}{
		{"category", spec.Category, active.Categories},
		{"action", spec.Action, active.Actions},
		{"urgency", spec.Urgency, active.Urgencies},
	} {
		if !isKnown(axis.values, axis.value) {
			log.Printf("[email] WARNING: card %q has %s %q outside the vocabulary (%s)",
				spec.Title, axis.name, axis.value, strings.Join(axis.values, ", "))
		}
	}

	column := columnForAction(spec.Action)

	// A due date the model reported in a shape parseDueDate cannot read is
	// logged and dropped rather than guessed at. Dropping it costs the card its
	// deadline-driven score; guessing costs the user a wrong one.
	var due *time.Time
	if spec.DueDate != "" {
		parsed, err := parseDueDate(spec.DueDate, time.Local)
		if err != nil {
			log.Printf("[email] WARNING: card %q has unusable due_date: %v", spec.Title, err)
		} else {
			due = &parsed
		}
	}

	score := scoreCard(spec.Category, spec.Action, spec.Urgency, due, time.Now())
	urgency := spec.Urgency
	if score.Urgency != "" {
		urgency = score.Urgency
	}

	tags := []string{
		tagEmail,
		tagSource,
		tagPrefixCat + spec.Category,
		tagPrefixAction + spec.Action,
		tagPrefixUrgent + urgency,
		workStateNotStarted,
	}
	if a.cfg.DryRun {
		log.Printf("[email] DRY-RUN create card %q [%s] cat=%s action=%s urgency=%s due=%s priority=%d",
			spec.Title, column, spec.Category, spec.Action, urgency, formatDue(due), score.Priority)
		// A synthetic id keeps the rest of the dry run coherent: filings that
		// reference this card still resolve, so the log shows the whole plan.
		id := "dry-run:" + normalizeTitle(spec.Title)
		a.state.Cards[id] = &cardState{ID: id, Title: spec.Title, ColumnName: column, Status: statusOpen, Tags: tags,
			Category: spec.Category, Action: spec.Action, Urgency: urgency,
			DueAt: due, Priority: score.Priority}
		// Counted here as well as on the real path, so the run summary a dry run
		// prints matches the one the same input would produce for real.
		a.cardsCreated++
		return id, nil
	}

	id, err := a.kc.CreateCard(issuesclient.CreateCardArgs{
		ColumnName: column,
		Title:      spec.Title,
		Body:       spec.Summary,
		Tags:       tags,
		Priority:   score.Priority,
		DueAt:      due,
		// Parked on arrival when the caller asked for it. Cards this classifier
		// files are noteboard todos, and an unattended worker discovering todos
		// cannot tell one that belongs to a pipeline from one nobody has claimed
		// — so without this it works whichever it finds first.
		Hold:       a.cfg.Classifier.HoldNewCards,
		HoldReason: holdReason,
	})
	if err != nil {
		return "", err
	}
	a.state.Cards[id] = &cardState{
		ID: id, Title: spec.Title, Body: spec.Summary, ColumnName: column, Status: statusOpen, Tags: tags,
		Category: spec.Category, Action: spec.Action, Urgency: urgency,
		DueAt: due, Priority: score.Priority,
	}
	a.cardsCreated++
	log.Printf("[email] created card %q [%s] cat=%s action=%s urgency=%s due=%s priority=%d id=%s",
		spec.Title, column, spec.Category, spec.Action, urgency, formatDue(due), score.Priority, id)
	return id, nil
}

// applyCardUpdate moves a card between action-state columns and refreshes its
// axis tags when new mail changed the answer.
func (a *applier) applyCardUpdate(upd cardUpdate) error {
	card, ok := a.state.Cards[upd.CardID]
	if !ok {
		return fmt.Errorf("card %s is not on this board", upd.CardID)
	}
	// A card a human has already marked completed must not be dragged back by a
	// late marketing follow-up.
	if card.ColumnName == columnActionCompleted {
		log.Printf("[email] card %q is in %s; leaving it alone", card.Title, columnActionCompleted)
		return nil
	}
	// Same reasoning one step further: an agent working the card is deciding its
	// action and column right now, and retagging or moving it underneath a live
	// session would fight whatever the agent is doing.
	if owned, session := a.gate.agentOwns(card); owned {
		log.Printf("[email] card %q is held by an agent — skipping retag/move (session=%s)", card.Title, session)
		a.skippedAgentOwned++
		return nil
	}

	// A card the sender rule already settled must not be raised again by a later
	// message from that same feed.
	if upd.Action != "" && upd.Action != actionNone && anySenderIsAlwaysNoAction(card.Senders) {
		log.Printf("[email] card %q is fed by a no-action sender; ignoring update to action=%q",
			card.Title, upd.Action)
		upd.Action = ""
	}

	newTags := card.Tags
	if upd.Action != "" && upd.Action != card.Action {
		newTags = replaceTag(newTags, tagPrefixAction, upd.Action)
		card.Action = upd.Action
	}
	if upd.Urgency != "" && upd.Urgency != card.Urgency {
		newTags = replaceTag(newTags, tagPrefixUrgent, upd.Urgency)
		card.Urgency = upd.Urgency
	}
	// An empty due_date leaves the stored deadline alone rather than clearing
	// it. noteboard keeps no revision history for due_at, so a clear is
	// unrecoverable, and the model omitting the field is far more often "this
	// mail said nothing about a date" than "the deadline was withdrawn".
	dueChanged := false
	if upd.DueDate != "" {
		parsed, err := parseDueDate(upd.DueDate, time.Local)
		if err != nil {
			log.Printf("[email] WARNING: card %q has unusable due_date in update: %v", card.Title, err)
		} else if card.DueAt == nil || !parsed.Equal(*card.DueAt) {
			card.DueAt = &parsed
			dueChanged = true
		}
	}

	// Re-score after the axes have settled, so the new deadline and the new
	// action are both reflected.
	score := scoreCard(card.Category, card.Action, card.Urgency, card.DueAt, time.Now())
	if score.Urgency != "" && score.Urgency != card.Urgency {
		newTags = replaceTag(newTags, tagPrefixUrgent, score.Urgency)
		card.Urgency = score.Urgency
	}
	priorityChanged := score.Priority != card.Priority
	card.Priority = score.Priority

	wantColumn := columnForAction(card.Action)

	if a.cfg.DryRun {
		log.Printf("[email] DRY-RUN update card %q -> action=%s urgency=%s due=%s priority=%d column=%s",
			card.Title, card.Action, card.Urgency, formatDue(card.DueAt), card.Priority, wantColumn)
		return nil
	}

	if !equalStringSlices(newTags, card.Tags) {
		if err := a.kc.PatchCard(card.ID, map[string]any{"tags": newTags}); err != nil {
			return fmt.Errorf("patch tags: %w", err)
		}
		card.Tags = newTags
	}
	// Sent as one PATCH: both fields live on the same noteboard item, and two
	// calls would leave a window where the deadline and its score disagree.
	if dueChanged || priorityChanged {
		fields := map[string]any{"priority": card.Priority}
		if dueChanged {
			fields["due_at"] = card.DueAt.Format(time.RFC3339)
		}
		if err := a.kc.PatchCard(card.ID, fields); err != nil {
			return fmt.Errorf("patch priority/due: %w", err)
		}
	}
	if wantColumn != card.ColumnName {
		colID, ok := a.kc.ColumnsByName[wantColumn]
		if !ok {
			return fmt.Errorf("column %q missing on board", wantColumn)
		}
		moveResult, err := a.kc.MoveCard(card.ID, a.kc.BoardID, colID, 0)
		if err != nil {
			return fmt.Errorf("move to %s: %w", wantColumn, err)
		}
		if moveResult.AutoStatusError != "" {
			log.Printf("[email] moved card %q to %s but its noteboard status was not set to match: %s", card.Title, wantColumn, moveResult.AutoStatusError)
		}
		log.Printf("[email] moved card %q: %s -> %s", card.Title, card.ColumnName, wantColumn)
		card.ColumnName = wantColumn
	}
	a.cardsUpdated++
	return nil
}

// fileToTriage is the last resort that keeps the invariant true when the model
// fails to place an email.
func (a *applier) fileToTriage(em pendingEmail) error {
	id, err := a.ensureTriageCard()
	if err != nil {
		return err
	}
	return a.linkEmail([]string{id}, em, emailLinkLabel(em.Meta, ""))
}

func (a *applier) ensureTriageCard() (string, error) {
	if a.triageCardID != "" {
		return a.triageCardID, nil
	}
	for id, c := range a.state.Cards {
		if c.Title == triageCardTitle {
			a.triageCardID = id
			return id, nil
		}
	}
	id, err := a.createCard(newCardSpec{
		Title:    triageCardTitle,
		Category: "personal",
		Action:   "decide",
		Urgency:  "normal",
		Summary: "Mail the classifier could not place on any card. Each entry here is a classifier " +
			"failure, not a task in itself — re-file them and the pattern that caused it is worth fixing.",
	})
	if err != nil {
		return "", err
	}
	a.triageCardID = id
	return id, nil
}

// ============================ card bodies ============================

// rewriteTouchedCardBodies refreshes the evidence list on every card that gained
// mail this tick. Only the region below evidenceMarker is replaced, so anything
// a human or the model wrote above it survives — rebuilding the whole body is
// how the sibling classifier erased its own provenance stamp.
func (a *applier) rewriteTouchedCardBodies() error {
	if a.cfg.DryRun {
		return nil
	}
	for cardID := range a.touched {
		card, ok := a.state.Cards[cardID]
		if !ok {
			continue
		}
		// The agent may be reading, or have edited, this body. New email links
		// were still attached above; the evidence section just renders stale
		// until the session ends, and the next tick after that re-renders it.
		if owned, session := a.gate.agentOwns(card); owned {
			log.Printf("[email] card %q is held by an agent — leaving its body alone (session=%s)", card.Title, session)
			a.skippedAgentOwned++
			continue
		}
		body := renderCardBody(card)
		if body == card.Body {
			continue
		}
		if err := a.kc.PatchCard(cardID, map[string]any{"body": body}); err != nil {
			log.Printf("[email] rewrite body for card %s: %v", cardID, err)
			continue
		}
		card.Body = body
	}
	return nil
}

func renderCardBody(card *cardState) string {
	prefix := card.Body
	if idx := strings.Index(prefix, evidenceMarker); idx >= 0 {
		prefix = prefix[:idx]
	}
	prefix = strings.TrimRight(prefix, "\n")

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("\n\n")
	b.WriteString(evidenceMarker)
	fmt.Fprintf(&b, "\n\n## Linked emails (%d)\n\n", len(card.EmailLinks))

	links := make([]issuesclient.CardLink, len(card.EmailLinks))
	copy(links, card.EmailLinks)
	sort.SliceStable(links, func(i, j int) bool { return links[i].Label < links[j].Label })
	for _, l := range links {
		label := l.Label
		if label == "" {
			label = "(no label)"
		}
		fmt.Fprintf(&b, "- %s — `%s`\n", label, l.EntityRef)
	}
	return b.String()
}

// ============================ small helpers ============================

// normalizeTitle folds a title to a comparison key: lowercase, non-alphanumeric
// runs collapsed to single spaces. Deliberately exact rather than fuzzy — a
// fuzzy match here would silently merge two genuinely different tasks.
func normalizeTitle(s string) string {
	var b strings.Builder
	lastWasSpace := false
	for _, r := range strings.ToLower(s) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			lastWasSpace = false
			continue
		}
		if !lastWasSpace {
			b.WriteRune(' ')
			lastWasSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// replaceTag swaps the value of the tag carrying prefix, appending it when the
// card had none.
func replaceTag(tags []string, prefix, value string) []string {
	out := make([]string, 0, len(tags)+1)
	replaced := false
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, t)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
