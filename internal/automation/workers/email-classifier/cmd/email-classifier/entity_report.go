package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// ============================ the -sender / -message pass ============================
//
// One command answering "what do I know about this address" and "what should the
// classifier know about it". Reading and writing sit in the same place on
// purpose: the note you want to write is almost always decided by the report you
// just read.

// senderMessageScanCap bounds how many messages one -sender report fetches.
//
// The address-to-message join is not stored anywhere and cannot be: mailstack has
// no search route, and kanban-store indexes entity-to-card, not tag-to-entity. So
// the only way to say which messages an address sent is to read the mail on the
// cards that address feeds. That is one live provider round trip per message, and
// a mature bucket card holds a hundred of them, so the walk is capped and says so
// rather than sitting there for two minutes.
const senderMessageScanCap = 120

func runEntityPass(cfg config, kc *issuesclient.Client, state *boardState) error {
	nb := newNotesClient(cfg.NotesURL)
	notes, err := loadRoutingNotes(nb)
	if err != nil {
		return fmt.Errorf("load routing notes: %w", err)
	}

	entityRef, err := entityRefFor(cfg)
	if err != nil {
		return err
	}

	if cfg.DeleteNote {
		return deleteNoteFor(nb, notes, entityRef, cfg.DryRun)
	}
	if cfg.NoteText != "" {
		if cfg.DryRun {
			log.Printf("[entity] DRY-RUN would write note on %s: %s", entityRef, cfg.NoteText)
			return nil
		}
		id, created, err := upsertRoutingNote(nb, notes, entityRef, cfg.NoteText)
		if err != nil {
			return err
		}
		verb := "updated"
		if created {
			verb = "created"
		}
		// The full id, because that is what resolves to a chip in the chat
		// surfaces and what a later PATCH needs.
		log.Printf("[entity] %s routing note %s on %s", verb, id, entityRef)
		return nil
	}
	return reportEntity(cfg, kc, state, notes, entityRef)
}

// entityRefFor turns the -sender / -message flags into one entity ref, refusing
// the ambiguous case rather than silently preferring one.
func entityRefFor(cfg config) (string, error) {
	sender := strings.ToLower(strings.TrimSpace(cfg.SenderQuery))
	message := strings.TrimSpace(cfg.MessageQuery)
	switch {
	case sender != "" && message != "":
		return "", fmt.Errorf("-sender and -message name two different things; pass one")
	case sender != "":
		if !strings.Contains(sender, "@") {
			return "", fmt.Errorf("-sender %q is not an email address", cfg.SenderQuery)
		}
		return entityTypeSenderPrefix + sender, nil
	case message != "":
		if _, _, ok := splitLocator(message); !ok {
			return "", fmt.Errorf("-message %q must be account:message_id, e.g. gmail-personal:19fc857e5e15e426", cfg.MessageQuery)
		}
		return entityTypeMessagePrefix + message, nil
	}
	return "", fmt.Errorf("pass -sender or -message")
}

func deleteNoteFor(nb *notesClient, notes *routingNotes, entityRef string, dryRun bool) error {
	var note routingNote
	var found bool
	if address, ok := strings.CutPrefix(entityRef, entityTypeSenderPrefix); ok {
		note, found = notes.forSender(address)
	} else if locator, ok := strings.CutPrefix(entityRef, entityTypeMessagePrefix); ok {
		note, found = notes.forMessage(locator)
	}
	if !found {
		log.Printf("[entity] no routing note on %s; nothing to delete", entityRef)
		return nil
	}
	if dryRun {
		log.Printf("[entity] DRY-RUN would delete routing note %s on %s", note.ItemID, entityRef)
		return nil
	}
	if err := deleteRoutingNote(nb, note); err != nil {
		return err
	}
	// notes' delete is reversible, and saying so is the difference between
	// a user who reaches for the restore route and one who retypes the note.
	log.Printf("[entity] deleted routing note %s on %s (reversible: POST /notes/items/%s/restore)",
		note.ItemID, entityRef, note.ItemID)
	return nil
}

// reportEntity prints what is known about one address or message.
func reportEntity(cfg config, kc *issuesclient.Client, state *boardState, notes *routingNotes, entityRef string) error {
	etype, eref, _ := strings.Cut(entityRef, ":")
	fmt.Printf("%s %s\n\n", strings.ToUpper(strings.ReplaceAll(etype, "_", " ")), eref)

	if note, ok := notes.forSender(eref); ok {
		fmt.Printf("routing note (%s):\n  %s\n\n", note.ItemID, note.Text)
	} else if note, ok := notes.forMessage(eref); ok {
		fmt.Printf("routing note (%s):\n  %s\n\n", note.ItemID, note.Text)
	} else {
		fmt.Printf("routing note: none — add one with:\n  email-classifier -%s %s -note \"…\"\n\n",
			map[bool]string{true: "sender", false: "message"}[etype == linkTypeEmailSender], eref)
	}

	cardIDs, err := kc.ListCardsForEntity(etype, eref)
	if err != nil {
		return fmt.Errorf("cards for %s: %w", entityRef, err)
	}
	if len(cardIDs) == 0 {
		fmt.Println("cards: none on this board")
		return nil
	}
	fmt.Printf("cards this entity appears on (%d):\n", len(cardIDs))
	cards := make([]*cardState, 0, len(cardIDs))
	for _, entry := range cardIDs {
		if c, ok := state.Cards[entry.CardID]; ok {
			cards = append(cards, c)
		}
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Title < cards[j].Title })
	for _, c := range cards {
		fmt.Printf("  [%s] %s — %d email(s)  card_id=%s\n", c.ColumnName, c.Title, len(c.EmailLinks), c.ID)
	}

	if etype != linkTypeEmailSender {
		return nil
	}
	reportMessagesFrom(cfg, cards, eref)
	return nil
}

// reportMessagesFrom names the messages this address actually sent, by reading
// the mail on the cards it feeds. See senderMessageScanCap for why this is a
// walk rather than a lookup.
func reportMessagesFrom(cfg config, cards []*cardState, address string) {
	mc := newMailClient(cfg.MailURL)
	fmt.Printf("\nmessages from %s (reading the mail on those cards, cap %d):\n", address, senderMessageScanCap)

	scanned, matched, failed := 0, 0, 0
	var truncated bool
	for _, c := range cards {
		for _, link := range c.EmailLinks {
			if scanned >= senderMessageScanCap {
				truncated = true
				break
			}
			accountID, messageID, ok := splitLocator(link.EntityRef)
			if !ok {
				continue
			}
			scanned++
			full, err := mc.getMessage(accountID, messageID)
			if err != nil {
				failed++
				continue
			}
			if senderKey(full.Meta.From) != address {
				continue
			}
			matched++
			fmt.Printf("  %s  %-55s [%s]\n",
				full.Meta.Date.Format("2006-01-02"), truncate(unescapeCommonEntities(full.Meta.Subject), 55), c.Title)
		}
		if truncated {
			break
		}
	}
	fmt.Printf("\n  %d message(s) from this address, out of %d read", matched, scanned)
	if failed > 0 {
		fmt.Printf("; %d could not be fetched", failed)
	}
	if truncated {
		fmt.Printf("; stopped at the %d-message cap, so there may be more", senderMessageScanCap)
	}
	fmt.Println()
}
