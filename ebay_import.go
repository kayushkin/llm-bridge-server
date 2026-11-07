package possessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// EbayConfirmationQuery is the Gmail search for eBay's order confirmations.
// eBay has titled them "✅ ORDER CONFIRMED: …" (to 2023), "Order confirmed:
// …" and "<name>, your order is confirmed"; Gmail matches the phrases in any
// case.
const EbayConfirmationQuery = `from:ebay@ebay.com (subject:"order confirmed" OR subject:"order is confirmed")`

// EbayImportRequest is POST /purchases/import-ebay-mail.
type EbayImportRequest struct {
	Actor   string `json:"actor"`
	Account string `json:"account"` // a mailstack account id
	Query   string `json:"query"`   // the provider's search; EbayConfirmationQuery when empty
}

var ebayOrderSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["is_order_confirmation","order_number","seller","total_cents","currency","lines","why"],"properties":{
"is_order_confirmation":{"type":"boolean","description":"Whether this email confirms an order the recipient placed (not a shipping or delivery update, an offer, an ad or a cart reminder)"},
"order_number":{"type":"string","description":"eBay's order number exactly as written (e.g. 12-34567-89012); empty when the email shows none"},
"seller":{"type":"string","description":"The seller's eBay user name, without the feedback count; empty when not shown"},
"total_cents":{"type":"integer","description":"The order total charged, in cents; -1 when not shown or not in the items' currency"},
"currency":{"type":"string","description":"ISO 4217 code of the items' prices (USD for $, GBP for GBP or £, EUR for €)"},
"lines":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["title","item_id","quantity","unit_price_cents"],"properties":{
  "title":{"type":"string","description":"The item's full title: the longest form the email shows, not one cut with …"},
  "item_id":{"type":"string","description":"eBay's item number, digits only, exactly as written"},
  "quantity":{"type":"integer"},
  "unit_price_cents":{"type":"integer","description":"The price of one, in cents; -1 when not shown"}}}},
"why":{"type":"string","description":"One sentence on anything unclear; empty when nothing is"}}}`)

const ebayOrderSystemPrompt = `You read eBay emails for a person's record of what they bought. Copy numbers and titles exactly as the email writes them; never guess one the email does not show. Ignore recommended items ("To complement your purchase", "more from this seller").`

type ebayOrderReading struct {
	IsOrderConfirmation bool   `json:"is_order_confirmation"`
	OrderNumber         string `json:"order_number"`
	Seller              string `json:"seller"`
	TotalCents          int64  `json:"total_cents"`
	Currency            string `json:"currency"`
	Lines               []struct {
		Title          string `json:"title"`
		ItemID         string `json:"item_id"`
		Quantity       int    `json:"quantity"`
		UnitPriceCents int64  `json:"unit_price_cents"`
	} `json:"lines"`
	Why string `json:"why"`
}

var ebayItemNumber = regexp.MustCompile(`^\d{9,15}$`)

// readEbayOrder asks the first reader what order a mail confirms, and
// checks every number it gives against the mail's text.
func (reviewer *Reviewer) readEbayOrder(ctx context.Context, mail MailMessage) (ebayOrderReading, ModelAnswer, error) {
	prompt := fmt.Sprintf("An email from %s, dated %s, subject %q:\n\n%s\n\nWhat order does it confirm?", mail.From, mail.Date.Format("2006-01-02 15:04 -0700"), mail.Subject, clip(mail.BodyText, 40000))
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, ebayOrderSystemPrompt, prompt, ebayOrderSchema, nil, 3000)
	if err != nil {
		return ebayOrderReading{}, answer, err
	}
	var reading ebayOrderReading
	if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
		return ebayOrderReading{}, answer, fmt.Errorf("%s answered %s: %w", answer.Model, answer.Parsed, err)
	}
	if !reading.IsOrderConfirmation {
		return reading, answer, nil
	}
	if reading.OrderNumber != "" && !strings.Contains(mail.BodyText, reading.OrderNumber) {
		return reading, answer, fmt.Errorf("%s read order number %q, which the email does not hold", answer.Model, reading.OrderNumber)
	}
	if len(reading.Lines) == 0 {
		return reading, answer, fmt.Errorf("%s read no item from an order confirmation", answer.Model)
	}
	for _, line := range reading.Lines {
		if !ebayItemNumber.MatchString(line.ItemID) || !strings.Contains(mail.BodyText, line.ItemID) {
			return reading, answer, fmt.Errorf("%s read item number %q, which is not an item number the email holds", answer.Model, line.ItemID)
		}
		if strings.TrimSpace(line.Title) == "" {
			return reading, answer, fmt.Errorf("%s read item %s with no title", answer.Model, line.ItemID)
		}
	}
	reading.Currency = strings.ToUpper(strings.TrimSpace(reading.Currency))
	if len(reading.Currency) != 3 {
		return reading, answer, fmt.Errorf("%s read currency %q, not an ISO 4217 code", answer.Model, reading.Currency)
	}
	return reading, answer, nil
}

// ImportEbayOrders reads every eBay order confirmation the search finds in
// one mailstack account and records each order as a purchase with its
// lines. A mail already imported is skipped, so running it again records
// only new orders. The first reader reads each mail (in parallel, four at
// a time); orders are written oldest first.
func (reviewer *Reviewer) ImportEbayOrders(ctx context.Context, request EbayImportRequest) (AuditRun, error) {
	run := newAuditRun()
	if request.Actor == "" {
		return run, fmt.Errorf("%w: say who is importing (the actor)", ErrInvalid)
	}
	if request.Account == "" {
		return run, fmt.Errorf("%w: account, a mailstack account id, is required", ErrInvalid)
	}
	if reviewer.Mail == nil {
		return run, fmt.Errorf("mailstack is not wired, so mail cannot be read")
	}
	query := request.Query
	if query == "" {
		query = EbayConfirmationQuery
	}
	ids := []string{}
	for cursor := ""; ; {
		page, next, err := reviewer.Mail.Search(ctx, request.Account, query, cursor)
		if err != nil {
			return run, err
		}
		ids = append(ids, page...)
		if next == "" || len(page) == 0 {
			break
		}
		cursor = next
	}
	run.Looked = len(ids)
	fresh := []string{}
	for _, id := range ids {
		recorded, err := stringColumn(reviewer.Store.db, `SELECT id FROM purchases WHERE mail_account_id = ? AND mail_message_id = ?`, request.Account, id)
		if err != nil {
			return run, err
		}
		if len(recorded) > 0 {
			run.Counts["already_imported"]++
			continue
		}
		fresh = append(fresh, id)
	}

	type reading struct {
		mail    MailMessage
		order   ebayOrderReading
		answer  ModelAnswer
		failure error
	}
	readings := make([]reading, len(fresh))
	work := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range work {
				mail, err := reviewer.Mail.Message(ctx, request.Account, fresh[index])
				if err != nil {
					readings[index] = reading{mail: MailMessage{ID: fresh[index]}, failure: err}
					continue
				}
				order, answer, err := reviewer.readEbayOrder(ctx, mail)
				readings[index] = reading{mail: mail, order: order, answer: answer, failure: err}
			}
		}()
	}
	for index := range fresh {
		work <- index
	}
	close(work)
	group.Wait()
	sort.SliceStable(readings, func(a, b int) bool { return readings[a].mail.Date.Before(readings[b].mail.Date) })

	for _, read := range readings {
		run.CostUSD += read.answer.CostUSD
		about := request.Account + ":" + read.mail.ID
		if read.failure != nil {
			run.Counts["failed"]++
			run.fail(about, read.failure)
			continue
		}
		if !read.order.IsOrderConfirmation {
			run.Counts["not_an_order"]++
			run.leave(about, fmt.Sprintf("%s says %q is not an order confirmation: %s", read.answer.Model, read.mail.Subject, read.order.Why))
			continue
		}
		purchase := Purchase{Seller: read.order.Seller, Marketplace: "ebay", OrderReference: read.order.OrderNumber, OrderedAt: read.mail.Date.Unix(),
			Currency: read.order.Currency, Source: "email_receipt", MailAccountID: request.Account, MailMessageID: read.mail.ID, ExtractedBy: read.answer.Model}
		if read.order.TotalCents >= 0 {
			total := read.order.TotalCents
			purchase.TotalCents = &total
		}
		for _, line := range read.order.Lines {
			recorded := PurchaseLine{Title: line.Title, Quantity: line.Quantity, ProductReference: line.ItemID, ListingURL: "https://www.ebay.com/itm/" + line.ItemID}
			if line.UnitPriceCents >= 0 {
				price := line.UnitPriceCents
				recorded.UnitPriceCents = &price
			}
			purchase.Lines = append(purchase.Lines, recorded)
		}
		created, err := reviewer.Store.CreatePurchase(ctx, purchase)
		if errors.Is(err, ErrConflict) {
			run.Counts["order_already_recorded"]++
			run.leave(about, err.Error())
			continue
		}
		if err != nil {
			run.Counts["failed"]++
			run.fail(about, err)
			continue
		}
		run.Counts["orders"]++
		run.Counts["lines"] += len(created.Lines)
		records := []string{created.ID}
		for _, line := range created.Lines {
			records = append(records, line.ID)
		}
		run.Actions = append(run.Actions, AuditAction{Action: "recorded", Records: records,
			Reason: fmt.Sprintf("%s order %s from %s, read from mail %s by %s", purchase.Marketplace, purchase.OrderReference, purchase.Seller, about, read.answer.Model)})
	}
	return run, nil
}
