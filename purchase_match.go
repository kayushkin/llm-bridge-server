package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	PurchaseLineID                            string
	score                                     int
}

// openPurchaseLine is a line no possession points at yet, with its order.
type openPurchaseLine struct {
	ID, PurchaseID, Title, Seller, Marketplace, Currency string
	OrderedAt                                            int64
	UnitPriceCents                                       *int64
}

func (s *Store) openPurchaseLines(purchaseID string) ([]openPurchaseLine, error) {
	query := `SELECT purchase_lines.id, purchases.id, purchase_lines.title, purchases.seller, purchases.marketplace, purchases.currency, purchases.ordered_at, purchase_lines.unit_price_cents
		FROM purchase_lines JOIN purchases ON purchases.id = purchase_lines.purchase_id
		WHERE purchases.deleted_at = 0 AND purchase_lines.disposition = 'unreviewed'
		AND NOT EXISTS (SELECT 1 FROM possessions WHERE possessions.purchase_line_id = purchase_lines.id AND possessions.deleted_at = 0)`
	arguments := []any{}
	if purchaseID != "" {
		query += ` AND purchases.id = ?`
		arguments = append(arguments, purchaseID)
	}
	rows, err := s.db.Query(query+` ORDER BY purchases.ordered_at, purchase_lines.line_number`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []openPurchaseLine{}
	for rows.Next() {
		var line openPurchaseLine
		var price sql.NullInt64
		if err := rows.Scan(&line.ID, &line.PurchaseID, &line.Title, &line.Seller, &line.Marketplace, &line.Currency, &line.OrderedAt, &price); err != nil {
			return nil, err
		}
		if price.Valid {
			line.UnitPriceCents = &price.Int64
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
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
	rows, err := s.db.Query(`SELECT id, name, brand, model, description, COALESCE(purchase_line_id, '') FROM possessions WHERE deleted_at = 0 ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []purchaseCandidate{}
	for rows.Next() {
		var candidate purchaseCandidate
		if err := rows.Scan(&candidate.ID, &candidate.Name, &candidate.Brand, &candidate.Model, &candidate.Description, &candidate.PurchaseLineID); err != nil {
			return nil, err
		}
		candidate.Kind = kinds[candidate.ID]
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// shortlist ranks possessions by the title's words they share: a word in
// the model counts 4, in the brand 3, in the name 2, in the description 1.
// Possessions already tied to an order line are left out.
func shortlist(title string, candidates []purchaseCandidate) []purchaseCandidate {
	words := titleWords(title)
	ranked := []purchaseCandidate{}
	for _, candidate := range candidates {
		if candidate.PurchaseLineID != "" {
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

func purchaseMatchPrompt(line openPurchaseLine, candidates []purchaseCandidate, pictureOf map[string]int) string {
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

// MatchPurchaseLines links order lines to the possessions they bought.
// For each line no possession points at, still unreviewed and not asked
// about before, both readers see the line and up to 8 possessions sharing
// its words, with their pictures. Both pick the same one and both are sure:
// the possession takes the line (and its price and the order's date when it
// has none). Either picks one otherwise: an optional question on that
// possession's photo, asked_by purchase_match. Both say none, or no
// possession shares its words: the line stays unreviewed.
func (reviewer *Reviewer) MatchPurchaseLines(ctx context.Context, actor, purchaseID string) (AuditRun, error) {
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: say who is matching (the actor)", ErrInvalid)
	}
	lines, err := reviewer.Store.openPurchaseLines(purchaseID)
	if err != nil {
		return run, err
	}
	candidates, err := reviewer.Store.purchaseCandidates()
	if err != nil {
		return run, err
	}
	pictures := newPictureCache(reviewer.Photos)
	for _, line := range lines {
		asked, err := questionAboutExists(reviewer.Store.db, `"action":"purchase_line_link"`, jsonField("purchase_line_id", line.ID))
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
			if err := reviewer.Store.LinkPurchaseLine(ctx, picks[0], line.ID, actor); err != nil {
				run.Counts["failed"]++
				run.fail(line.ID, err)
				continue
			}
			for index := range candidates {
				if candidates[index].ID == picks[0] {
					candidates[index].PurchaseLineID = line.ID
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
func (s *Store) askAboutPurchaseLine(ctx context.Context, line openPurchaseLine, picks [2]string, said [2]string, shortlisted []purchaseCandidate) (AuditAction, error) {
	names := map[string]string{}
	for _, candidate := range shortlisted {
		names[candidate.ID] = candidate.Name
	}
	distinct := []string{}
	for _, pick := range picks {
		if pick != "" && !contains(distinct, pick) {
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
			Resolution: Resolution{Action: "purchase_line_link", PossessionID: pick, PurchaseLineID: line.ID}})
	}
	choices = append(choices, Choice{Label: "No, not from this order", Resolution: Resolution{Action: "purchase_line_not_this", PurchaseLineID: line.ID}})
	questionID, err := insertQuestionAnsweringID(transaction, sightingID, question, choices, "detail", s.unixNow())
	if err != nil {
		return AuditAction{}, err
	}
	if _, err := transaction.Exec(`UPDATE owner_questions SET asked_by = 'purchase_match' WHERE id = ?`, questionID); err != nil {
		return AuditAction{}, err
	}
	return AuditAction{Action: "asked", Records: append([]string{line.ID}, distinct...), QuestionID: questionID, Reason: question}, transaction.Commit()
}

// LinkPurchaseLine records that a possession came from an order line.
func (s *Store) LinkPurchaseLine(ctx context.Context, possessionID, lineID, actor string) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := s.linkPurchaseLineInTransaction(ctx, transaction, possessionID, lineID, actor, s.unixNow()); err != nil {
		return err
	}
	return transaction.Commit()
}

// linkPurchaseLineInTransaction points the possession at the line, fills
// its acquired date, how and price where it has none, and marks the line a
// possession. A possession already from another line is a conflict.
func (s *Store) linkPurchaseLineInTransaction(ctx context.Context, transaction *sql.Tx, possessionID, lineID, actor string, now int64) error {
	possession, err := possessionView(transaction, possessionID, false)
	if err != nil {
		return err
	}
	if existing, _ := possession["purchase_line_id"].(*string); existing != nil && *existing != lineID {
		return fmt.Errorf("%w: %s already came from %s", ErrConflict, possessionID, *existing)
	}
	if existing, _ := possession["purchase_line_id"].(string); existing != "" && existing != lineID {
		return fmt.Errorf("%w: %s already came from %s", ErrConflict, possessionID, existing)
	}
	var orderedAt int64
	var currency string
	var price sql.NullInt64
	if err := transaction.QueryRow(`SELECT purchases.ordered_at, purchases.currency, purchase_lines.unit_price_cents FROM purchase_lines JOIN purchases ON purchases.id = purchase_lines.purchase_id WHERE purchase_lines.id = ?`, lineID).
		Scan(&orderedAt, &currency, &price); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no purchase line %s", ErrInvalid, lineID)
		}
		return err
	}
	fields := map[string]json.RawMessage{}
	fields["purchase_line_id"], _ = json.Marshal(lineID)
	if possession["acquired_at"] == int64(0) {
		fields["acquired_at"], _ = json.Marshal(orderedAt)
	}
	if possession["acquired_how"] == "" {
		fields["acquired_how"], _ = json.Marshal("bought")
	}
	if possession["price_cents"] == nil && price.Valid && len(currency) == 3 {
		fields["price_cents"], _ = json.Marshal(price.Int64)
		fields["price_currency"], _ = json.Marshal(currency)
	}
	if err := s.patchPossessionInTransaction(ctx, transaction, possessionID, fields, nil, nil, actor); err != nil {
		return err
	}
	if err := saveRevision(transaction, "purchase_lines", "id", lineID, actor, now); err != nil {
		return err
	}
	_, err = transaction.Exec(`UPDATE purchase_lines SET disposition = 'possession' WHERE id = ?`, lineID)
	return err
}
