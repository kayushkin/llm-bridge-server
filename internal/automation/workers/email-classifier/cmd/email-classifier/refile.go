package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// ============================ refile pass ============================
//
// Mail that the retired sender rule placed was never fetched and never read: the
// rule sent an address's mail to whichever bucket card their previous message
// landed on and stopped there. This pass finds that mail and unfiles it, so the
// ordinary tick classifies it the way everything else is classified now.
//
// The test for "never read" is exact, not a guess about link labels. A message
// that was fetched had its RFC 5322 Message-ID recorded as its own link on the
// card it landed on; a message the sender rule placed has no such link, because
// the rule short-circuited before the fetch. So fetch the message now: if its
// Message-ID is absent from the card holding it, that card never saw it.
//
// This pass only UNLINKS. Re-classification is the ordinary tick's job, which
// already collects any message with no card link and drains at a bounded rate.
// Splitting it that way means the destructive half is small enough to read, and
// a -dry-run tells you exactly what it would take apart before it does.
//
// Two things it deliberately leaves alone:
//
//   - Cards an agent is working on. Yanking evidence out from under a running
//     session is worse than leaving one message filed where it should not be.
//   - The email_sender links. They are evidence of which addresses have fed a
//     card, that remains true of the past whether or not the mail still hangs
//     there, and pruning them accurately would mean fetching every remaining
//     message on the card to find out who sent it.

// refileCandidate is one email link that was attached without the message ever
// being read.
type refileCandidate struct {
	Card *cardState
	Link kanbanclient.CardLink
	// Subject and Sender come from the fetch this pass had to do anyway, and
	// exist so the log names the mail rather than an opaque locator.
	Subject string
	Sender  string
}

// findMailFiledUnread returns the email links attached on or after `since` whose
// message the card holding them never saw.
//
// A message that cannot be fetched is left alone and reported. Unlinking mail we
// cannot re-read would drop it off the board entirely: the tick would have to
// fetch it to re-file it, and the fetch is what just failed.
func findMailFiledUnread(mc *mailClient, state *boardState, gate *sessionGate, since time.Time) ([]refileCandidate, int, int) {
	cardIDs := make([]string, 0, len(state.Cards))
	for id := range state.Cards {
		cardIDs = append(cardIDs, id)
	}
	// Deterministic order, so two runs over an unchanged board print the same
	// report and a diff between them means something.
	sort.Strings(cardIDs)

	var candidates []refileCandidate
	read, unreachable := 0, 0

	for _, cardID := range cardIDs {
		card := state.Cards[cardID]
		if owned, session := gate.agentOwns(card); owned {
			log.Printf("[refile] card %q is held by an agent — leaving its mail alone (session=%s)", card.Title, session)
			continue
		}
		// The Message-IDs this card is known to have seen.
		held := map[string]bool{}
		for ref, cards := range state.CardsByMessageID {
			for _, id := range cards {
				if id == cardID {
					held[ref] = true
				}
			}
		}

		for _, link := range card.EmailLinks {
			if link.CreatedAt.Before(since) {
				continue
			}
			accountID, messageID, ok := splitLocator(link.EntityRef)
			if !ok {
				log.Printf("[refile] WARNING: card %q holds an email link with an unparseable locator %q; leaving it",
					card.Title, link.EntityRef)
				continue
			}
			full, err := mc.getMessage(accountID, messageID)
			if err != nil {
				log.Printf("[refile] fetch %s: %v — leaving it filed, since unfiling mail we cannot re-read would lose it",
					link.EntityRef, err)
				unreachable++
				continue
			}
			if msgID := messageIDOf(full); msgID != "" && held[msgID] {
				read++
				continue
			}
			candidates = append(candidates, refileCandidate{
				Card:    card,
				Link:    link,
				Subject: full.Meta.Subject,
				Sender:  senderKey(full.Meta.From),
			})
		}
	}
	return candidates, read, unreachable
}

// splitLocator reverses locatorOf.
func splitLocator(locator string) (accountID, messageID string, ok bool) {
	accountID, messageID, ok = strings.Cut(locator, ":")
	if !ok || accountID == "" || messageID == "" {
		return "", "", false
	}
	return accountID, messageID, true
}

func runRefileUnreadPass(cfg config, kc *kanbanclient.Client, state *boardState, gate *sessionGate, since time.Time) error {
	mc := newMailClient(cfg.MailURL, cfg.MailToken)

	log.Printf("[refile] looking for mail filed without being read since %s", since.Format("2006-01-02"))
	candidates, read, unreachable := findMailFiledUnread(mc, state, gate, since)
	log.Printf("[refile] filed-and-read=%d filed-unread=%d unreachable=%d", read, len(candidates), unreachable)

	byCard := map[string]int{}
	for _, c := range candidates {
		byCard[c.Card.ID]++
		log.Printf("[refile] %s | %s | %s -> %q",
			c.Link.CreatedAt.Format("2006-01-02"), c.Sender, truncate(c.Subject, 55), c.Card.Title)
	}
	if len(candidates) == 0 {
		log.Printf("[refile] nothing to unfile")
		return nil
	}
	if cfg.DryRun {
		log.Printf("[refile] DRY-RUN: would unfile %d message(s) from %d card(s); the next ticks would re-classify them",
			len(candidates), len(byCard))
		reportEmptiedCards(state, byCard)
		return nil
	}

	applier := &applier{cfg: cfg, kc: kc, state: state, gate: gate}
	unfiled := 0
	for _, c := range candidates {
		if err := kc.DeleteCardLink(c.Link.ID); err != nil {
			// Keep going: one link the store refuses must not strand the rest,
			// and this pass is safe to run again.
			log.Printf("[refile] WARNING: unlink %s from %q: %v", c.Link.EntityRef, c.Card.Title, err)
			continue
		}
		c.Card.EmailLinks = removeLinkByID(c.Card.EmailLinks, c.Link.ID)
		delete(state.FiledLocators, c.Link.EntityRef)
		applier.markTouched(c.Card.ID)
		unfiled++
	}

	// The evidence list in each card's body is rendered from EmailLinks, so a
	// card that just lost mail has to be re-rendered or its body goes on listing
	// messages the card no longer holds.
	if err := applier.rewriteTouchedCardBodies(); err != nil {
		return fmt.Errorf("rewrite card bodies: %w", err)
	}
	reportEmptiedCards(state, byCard)
	log.Printf("[refile] unfiled %d message(s) from %d card(s)", unfiled, len(byCard))
	// An ordinary tick stops walking at the first page where everything is
	// already filed, so mail unfiled from further back than that page reaches is
	// invisible to it and would sit off the board indefinitely. Measured: of 124
	// messages unfiled over five days, three were on a deeper page and only a
	// backfill walk found them.
	log.Printf("[refile] drain with `email-classifier -backfill` — a plain tick stops at the first fully-filed page and will miss anything older than it")
	return nil
}

// reportEmptiedCards names the cards this pass leaves holding no mail at all.
//
// Such a card is not deleted here. It may be a bucket a human named and wants
// kept, and the model will offer it in the next digest either way — but a card
// with no evidence left on it is a decision someone should look at, so it is
// said out loud rather than left to be noticed.
func reportEmptiedCards(state *boardState, byCard map[string]int) {
	for cardID, removed := range byCard {
		card, ok := state.Cards[cardID]
		if !ok {
			continue
		}
		if len(card.EmailLinks) == 0 {
			log.Printf("[refile] NOTE: card %q now holds no mail (all %d of its messages were filed unread)",
				card.Title, removed)
		}
	}
}

func removeLinkByID(links []kanbanclient.CardLink, id string) []kanbanclient.CardLink {
	out := links[:0]
	for _, l := range links {
		if l.ID != id {
			out = append(out, l)
		}
	}
	return out
}
