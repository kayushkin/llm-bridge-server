package possessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge/msg"
)

// The most possessions shown to the readers for one purchase line, each
// with its cover picture.
const purchaseMatchCandidates = 8

// purchaseTitleNoise are words in marketplace titles that say nothing about
// which thing it is.
var purchaseTitleNoise = map[string]bool{
	"new": true, "used": true, "vintage": true, "vtg": true, "the": true, "and": true, "for": true, "with": true, "from": true, "size": true,
	"mens": true, "men": true, "women": true, "womens": true, "unisex": true, "adult": true, "complete": true, "box": true, "lot": true,
	"original": true, "official": true, "authentic": true, "rare": true, "edition": true, "free": true, "shipping": true, "excellent": true,
	"condition": true, "made": true, "nwt": true, "nib": true, "great": true, "good": true, "pre": true, "owned": true,
}

func titleWords(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	kept := []string{}
	seen := map[string]bool{}
	for _, word := range words {
		if len(word) < 3 || purchaseTitleNoise[word] || seen[word] {
			continue
		}
		seen[word] = true
		kept = append(kept, word)
	}
	return kept
}

type purchaseCandidate struct {
	ID, Name, Brand, Model, Description, Kind string
	// Bought says the possession is already tied to an order: a budget-store
	// line links to it, or it names the transaction that paid for it.
	Bought bool
	score  int
}

// openOrderLine is a budget-store order line no possession is linked from
// yet, with its order.
type openOrderLine struct {
	ID, Title, ItemNumber, Seller, Marketplace, Currency, TransactionID string
	OrderedAt                                                           int64
	UnitPriceCents                                                      *int64
}

func openOrderLines(orders []BudgetOrder, orderID string) []openOrderLine {
	lines := []openOrderLine{}
	for _, order := range orders {
		if orderID != "" && order.ID != orderID {
			continue
		}
		for _, line := range order.Lines {
			if line.Disposition != "unreviewed" || len(line.PossessionIDs) > 0 {
				continue
			}
			lines = append(lines, openOrderLine{ID: line.ID, Title: line.Title, ItemNumber: line.ItemNumber, Seller: order.Seller, Marketplace: order.Marketplace,
				Currency: order.Currency, TransactionID: order.TransactionID, OrderedAt: order.OrderedAt, UnitPriceCents: line.UnitPriceCents})
		}
	}
	sort.SliceStable(lines, func(a, b int) bool { return lines[a].OrderedAt < lines[b].OrderedAt })
	return lines
}

// purchaseCandidates reads every live possession, with its kind, once per
// pass.
func (s *Store) purchaseCandidates() ([]purchaseCandidate, error) {
	kinds := map[string]string{}
	for _, kind := range KindTypes {
		ids, err := stringColumn(s.db, `SELECT possession_id FROM `+kind.Table)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			kinds[id] = kind.Name
		}
	}
	rows, err := s.db.Query(`SELECT id, name, brand, model, description, budget_transaction_id IS NOT NULL OR purchase_line_id IS NOT NULL FROM possessions WHERE deleted_at = 0 ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []purchaseCandidate{}
	for rows.Next() {
		var candidate purchaseCandidate
		if err := rows.Scan(&candidate.ID, &candidate.Name, &candidate.Brand, &candidate.Model, &candidate.Description, &candidate.Bought); err != nil {
			return nil, err
		}
		candidate.Kind = kinds[candidate.ID]
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// shortlist ranks possessions by the title's words they share: a word in
// the model counts 4, in the brand 3, in the name 2, in the description 1.
// Possessions already tied to an order are left out.
func shortlist(title string, candidates []purchaseCandidate) []purchaseCandidate {
	words := titleWords(title)
	ranked := []purchaseCandidate{}
	for _, candidate := range candidates {
		if candidate.Bought {
			continue
		}
		model, brand, name, description := wordSet(candidate.Model), wordSet(candidate.Brand), wordSet(candidate.Name), wordSet(candidate.Description)
		score := 0
		for _, word := range words {
			switch {
			case model[word]:
				score += 4
			case brand[word]:
				score += 3
			case name[word]:
				score += 2
			case description[word]:
				score++
			}
		}
		if score >= 2 {
			candidate.score = score
			ranked = append(ranked, candidate)
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })
	if len(ranked) > purchaseMatchCandidates {
		ranked = ranked[:purchaseMatchCandidates]
	}
	return ranked
}

func wordSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, word := range titleWords(text) {
		set[word] = true
	}
	return set
}

var purchaseMatchSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["match","sure","why"],"properties":{
"match":{"type":"string","description":"The label (C1, C2…) of the possession this order line bought, or none"},
"sure":{"type":"boolean","description":"True only when the title and the picture leave no real doubt: the same make and model, or the same distinctive thing"},
"why":{"type":"string","description":"One sentence"}}}`)

const purchaseMatchSystemPrompt = `You link a person's online orders to the things in their inventory. An order line is a seller's title; the inventory entries were written from photos of the person's home and outfits. Match only the thing the order bought: the same make and model, or the same distinctive item; a similar thing is not a match. Many orders were gifts, used up, returned or never photographed, so "none" is often right.`

func purchaseMatchPrompt(line openOrderLine, candidates []purchaseCandidate, pictureOf map[string]int) string {
	var prompt strings.Builder
	price := "price not shown"
	if line.UnitPriceCents != nil {
		price = fmt.Sprintf("%.2f %s", float64(*line.UnitPriceCents)/100, line.Currency)
	}
	fmt.Fprintf(&prompt, "Order line, bought on %s from seller %q on %s, %s:\n  %s\n\nInventory entries it could be:\n",
		line.Marketplace, line.Seller, time.Unix(line.OrderedAt, 0).UTC().Format("2006-01-02"), price, line.Title)
	for index, candidate := range candidates {
		fmt.Fprintf(&prompt, "C%d: %s", index+1, candidate.Name)
		if candidate.Kind != "" {
			fmt.Fprintf(&prompt, " [%s]", candidate.Kind)
		}
		if candidate.Brand != "" || candidate.Model != "" {
			fmt.Fprintf(&prompt, ", make %s %s", candidate.Brand, candidate.Model)
		}
		if picture, has := pictureOf[candidate.ID]; has {
			fmt.Fprintf(&prompt, " (picture %d)", picture)
		} else {
			prompt.WriteString(" (no picture)")
		}
		if candidate.Description != "" {
			fmt.Fprintf(&prompt, "\n    %s", clip(strings.ReplaceAll(candidate.Description, "\n", " "), 400))
		}
		prompt.WriteString("\n")
	}
	prompt.WriteString("\nThe pictures come in the order their numbers say. Which entry, if any, did this order line buy?")
	return prompt.String()
}

// MatchOrderLines links budget-store's marketplace order lines to the
// possessions they bought. For each line no possession is linked from,
// still unreviewed and not asked about before, both readers see the line
// and up to 8 possessions sharing its words, with their pictures. Both pick
// the same one and both are sure: linked (LinkOrderLine). Either picks one
// otherwise: an optional question on that possession's photo, asked_by
// purchase_match. Both say none, or no possession shares its words: the line
// stays unreviewed.
func (reviewer *Reviewer) MatchOrderLines(ctx context.Context, actor, orderID string) (AuditRun, error) {
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: say who is matching (the actor)", ErrInvalid)
	}
	if reviewer.Budget == nil {
		return run, fmt.Errorf("budget-store is not wired, so order lines cannot be read")
	}
	orders, err := reviewer.Budget.ListOrders(ctx)
	if err != nil {
		return run, err
	}
	lines := openOrderLines(orders, orderID)
	candidates, err := reviewer.Store.purchaseCandidates()
	if err != nil {
		return run, err
	}
	linked := map[string]bool{}
	for _, order := range orders {
		for _, line := range order.Lines {
			for _, possessionID := range line.PossessionIDs {
				linked[possessionID] = true
			}
		}
	}
	for index := range candidates {
		candidates[index].Bought = candidates[index].Bought || linked[candidates[index].ID]
	}
	pictures := newPictureCache(reviewer.Photos)
	for _, line := range lines {
		asked, err := questionAboutExists(reviewer.Store.db, `"action":"order_line_link"`, jsonField("marketplace_order_line_id", line.ID))
		if err != nil {
			return run, err
		}
		if asked {
			run.leave(line.ID, "asked about before")
			continue
		}
		run.Looked++
		shortlisted := shortlist(line.Title, candidates)
		if len(shortlisted) == 0 {
			run.Counts["unmatched"]++
			run.leave(line.ID, "no possession shares a word with "+fmt.Sprintf("%q", line.Title))
			continue
		}
		images := []msg.OneShotImage{}
		pictureOf := map[string]int{}
		for _, candidate := range shortlisted {
			picture, err := pictures.coverCrop(ctx, reviewer.Store.db, candidate.ID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				run.fail(candidate.ID, err)
				continue
			}
			images = append(images, jpegImage(picture))
			pictureOf[candidate.ID] = len(images)
		}
		answers, err := reviewer.askBothReaders(ctx, purchaseMatchSystemPrompt, purchaseMatchPrompt(line, shortlisted, pictureOf), purchaseMatchSchema, images, 800)
		run.CostUSD += answers[0].CostUSD + answers[1].CostUSD
		if err != nil {
			run.Counts["failed"]++
			run.fail(line.ID, err)
			continue
		}
		picks := [2]string{}
		sure := [2]bool{}
		said := [2]string{}
		for index, answer := range answers {
			var reading struct {
				Match string `json:"match"`
				Sure  bool   `json:"sure"`
				Why   string `json:"why"`
			}
			if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
				run.fail(line.ID, fmt.Errorf("%s answered %s: %w", answer.Model, answer.Parsed, err))
				continue
			}
			var number int
			if _, err := fmt.Sscanf(strings.ToUpper(strings.TrimSpace(reading.Match)), "C%d", &number); err == nil && number >= 1 && number <= len(shortlisted) {
				picks[index] = shortlisted[number-1].ID
			}
			sure[index] = reading.Sure
			said[index] = fmt.Sprintf("%s: %s (%s)", answer.Model, orNone(picks[index]), reading.Why)
		}
		switch {
		case picks[0] != "" && picks[0] == picks[1] && sure[0] && sure[1]:
			if err := reviewer.Store.LinkOrderLine(ctx, picks[0], line.ID, line.TransactionID, actor, "both readers: "+strings.Join(said[:], "; ")); err != nil {
				run.Counts["failed"]++
				run.fail(line.ID, err)
				continue
			}
			for index := range candidates {
				if candidates[index].ID == picks[0] {
					candidates[index].Bought = true
				}
			}
			run.Counts["matched"]++
			run.Actions = append(run.Actions, AuditAction{Action: "linked", Records: []string{line.ID, picks[0]}, Reason: strings.Join(said[:], "; ")})
		case picks[0] == "" && picks[1] == "":
			run.Counts["unmatched"]++
			run.leave(line.ID, "both readers say none: "+strings.Join(said[:], "; "))
		default:
			action, err := reviewer.Store.askAboutPurchaseLine(ctx, line, picks, said, shortlisted)
			if err != nil {
				run.Counts["failed"]++
				run.fail(line.ID, err)
				continue
			}
			if action.Action == "" {
				run.Counts["unmatched"]++
				run.leave(line.ID, action.Reason)
				continue
			}
			run.Counts["asked"]++
			run.Actions = append(run.Actions, action)
		}
	}
	return run, nil
}

func orNone(id string) string {
	if id == "" {
		return "none"
	}
	return id
}

// askAboutPurchaseLine asks the owner whether the order line bought the
// possession either reader picked, on that possession's photo.
func (s *Store) askAboutPurchaseLine(ctx context.Context, line openOrderLine, picks [2]string, said [2]string, shortlisted []purchaseCandidate) (AuditAction, error) {
	names := map[string]string{}
	for _, candidate := range shortlisted {
		names[candidate.ID] = candidate.Name
	}
	distinct := []string{}
	for _, pick := range picks {
		if pick != "" && !slices.Contains(distinct, pick) {
			distinct = append(distinct, pick)
		}
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuditAction{}, err
	}
	defer transaction.Rollback()
	sightingID := ""
	for _, pick := range distinct {
		if sightingID, err = sightingShowing(transaction, pick); err != nil {
			return AuditAction{}, err
		}
		if sightingID != "" {
			break
		}
	}
	if sightingID == "" {
		return AuditAction{Reason: "the readers' pick was not recorded from a photo, so there is no sighting to ask on: " + strings.Join(said[:], "; ")}, nil
	}
	question := fmt.Sprintf("Did your %s order line %s, %q from %s, buy this? The readers said: %s; %s.", line.Marketplace, line.ID, line.Title, line.Seller, said[0], said[1])
	choices := []Choice{}
	for _, pick := range distinct {
		choices = append(choices, Choice{Label: fmt.Sprintf("Yes: %s (%s) came from this order", names[pick], pick),
			Resolution: Resolution{Action: "order_line_link", PossessionID: pick, MarketplaceOrderLineID: line.ID, BudgetTransactionID: line.TransactionID}})
	}
	choices = append(choices, Choice{Label: "No, not from this order", Resolution: Resolution{Action: "order_line_not_this", MarketplaceOrderLineID: line.ID}})
	questionID, err := insertQuestionAnsweringID(transaction, sightingID, question, choices, "detail", s.unixNow())
	if err != nil {
		return AuditAction{}, err
	}
	if _, err := transaction.Exec(`UPDATE owner_questions SET asked_by = 'purchase_match' WHERE id = ?`, questionID); err != nil {
		return AuditAction{}, err
	}
	return AuditAction{Action: "asked", Records: append([]string{line.ID}, distinct...), QuestionID: questionID, Reason: question}, transaction.Commit()
}

// LinkOrderLine records that a possession came from a budget-store order
// line: the possession names the transaction that paid for it (when the
// order has one), takes the order's date, "bought" and the line's price
// where it has none, and budget-store links the line to it. A possession
// already paid by another transaction is a conflict.
func (s *Store) LinkOrderLine(ctx context.Context, possessionID, lineID, transactionID, actor, note string) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := s.linkOrderLineInTransaction(ctx, transaction, possessionID, lineID, transactionID, actor, note); err != nil {
		return err
	}
	return transaction.Commit()
}

func (s *Store) linkOrderLineInTransaction(ctx context.Context, transaction *databaseTransaction, possessionID, lineID, transactionID, actor, note string) error {
	if s.budget == nil {
		return fmt.Errorf("budget-store is not wired, so an order line cannot be linked")
	}
	order, line, err := s.budget.OrderOfLine(ctx, lineID)
	if err != nil {
		return err
	}
	if transactionID != order.TransactionID {
		// The order found its charge, or lost it, since the question was asked.
		transactionID = order.TransactionID
	}
	possession, err := possessionView(transaction, possessionID, false)
	if err != nil {
		return err
	}
	existing := ""
	switch value := possession["budget_transaction_id"].(type) {
	case string:
		existing = value
	case *string:
		if value != nil {
			existing = *value
		}
	}
	if existing != "" && transactionID != "" && existing != transactionID {
		return fmt.Errorf("%w: %s was already paid by %s", ErrConflict, possessionID, existing)
	}
	fields := map[string]json.RawMessage{}
	if transactionID != "" && existing == "" {
		fields["budget_transaction_id"], _ = json.Marshal(transactionID)
	}
	if possession["acquired_at"] == int64(0) {
		fields["acquired_at"], _ = json.Marshal(order.OrderedAt)
	}
	if possession["acquired_how"] == "" {
		fields["acquired_how"], _ = json.Marshal("bought")
	}
	if possession["price_cents"] == nil && line.UnitPriceCents != nil && len(order.Currency) == 3 {
		fields["price_cents"], _ = json.Marshal(*line.UnitPriceCents)
		fields["price_currency"], _ = json.Marshal(order.Currency)
	}
	if len(fields) > 0 {
		if err := s.patchPossessionInTransaction(ctx, transaction, possessionID, fields, nil, nil, actor); err != nil {
			return err
		}
	}
	return s.budget.LinkPossession(ctx, lineID, possessionID, note)
}
