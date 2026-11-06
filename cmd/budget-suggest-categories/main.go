// Command budget-suggest-categories asks a model to suggest a category for
// each transaction budget-store holds as Uncategorized, and records the
// suggestions there for the operator to accept, change or reject. It writes
// no category onto any transaction itself.
//
// Each batch is one classification.run operation on llm-bridge-server, with
// the store's own categories as the only answers allowed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	budgetstore "github.com/kayushkin/budget-store"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// needsOperatorValue is the answer a model gives when only the operator can
// say what a transaction was for.
const needsOperatorValue = "Needs the operator"

const categoryAxis = "category"

func main() {
	limit := flag.Int("limit", 200, "the most transactions to categorize in this run")
	batchSize := flag.Int("batch", 100, "transactions per classification.run operation")
	maximumCostPerBatch := flag.Float64("max-cost-per-batch", 2, "list-price ceiling in US dollars for one operation")
	flag.Parse()

	settings, err := newSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("read settings: %v", err)
	}
	run := runner{
		budgetStoreURL: strings.TrimRight(settings.String(settingBudgetStoreURL), "/"),
		bridgeURL:      strings.TrimRight(settings.String(settingBridgeURL), "/"),
		bridgeToken:    settings.String(settingBridgeToken),
		organizationID: settings.String(settingOrganizationID),
		modelRole:      settings.String(settingModelRole),
		client:         &http.Client{Timeout: 60 * time.Second},
	}

	var candidates []budgetstore.SuggestionCandidate
	if err := run.getJSON(fmt.Sprintf("%s/category-suggestions/candidates?limit=%d", run.budgetStoreURL, *limit), &candidates); err != nil {
		log.Fatalf("read candidates: %v", err)
	}
	if len(candidates) == 0 {
		log.Printf("nothing to categorize")
		return
	}
	taxonomy, categoryByValue, err := run.taxonomy()
	if err != nil {
		log.Fatalf("build taxonomy: %v", err)
	}
	log.Printf("categorizing %d transactions against %d categories, %d per batch, model role %s", len(candidates), len(categoryByValue), *batchSize, run.modelRole)
	runKey := time.Now().UTC().Format("20060102T150405")
	for start := 0; start < len(candidates); start += *batchSize {
		batch := candidates[start:min(start+*batchSize, len(candidates))]
		created, err := run.categorize(runKey, start / *batchSize, batch, taxonomy, categoryByValue, *maximumCostPerBatch)
		if err != nil {
			log.Fatalf("batch starting at %d: %v", start, err)
		}
		log.Printf("batch %d: %d suggestions recorded", start / *batchSize, created)
	}
}

type runner struct {
	budgetStoreURL, bridgeURL, bridgeToken, organizationID, modelRole string
	client                                                            *http.Client
}

// taxonomy offers every expense and income category by its path, plus the
// answer that sends a transaction to the operator. It returns the map from
// each offered value back to the category id it stands for; the map lives
// only for this run, built from the same read.
func (r runner) taxonomy() (msg.ClassificationTaxonomy, map[string]string, error) {
	var categories []budgetstore.Category
	if err := r.getJSON(r.budgetStoreURL+"/categories", &categories); err != nil {
		return msg.ClassificationTaxonomy{}, nil, err
	}
	var paths map[string]string
	if err := r.getJSON(r.budgetStoreURL+"/category-paths", &paths); err != nil {
		return msg.ClassificationTaxonomy{}, nil, err
	}
	categoryByValue := map[string]string{}
	var values []msg.ClassificationValue
	for _, c := range categories {
		if c.ArchivedAt != 0 || (c.Kind != "expense" && c.Kind != "income") {
			continue
		}
		path := paths[c.ID]
		if _, taken := categoryByValue[path]; taken {
			return msg.ClassificationTaxonomy{}, nil, fmt.Errorf("two categories share the path %q; rename one", path)
		}
		categoryByValue[path] = c.ID
		description := "money spent"
		if c.Kind == "income" {
			description = "money received"
		}
		values = append(values, msg.ClassificationValue{ID: c.ID, Name: path, Description: description})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	values = append(values, msg.ClassificationValue{Name: needsOperatorValue,
		Description: "Only the account holder can say what this was for: a payment to or from a person (Venmo, Zelle, Cash App, a PayPal transfer to a person, a check), a transfer whose other side is unclear, or a merchant you cannot identify from the text. A PayPal or Square charge that names a merchant is a purchase from that merchant, not this."})
	taxonomy := msg.ClassificationTaxonomy{
		Name:   "budget categories",
		Domain: "one transaction from a person's own checking account, credit card or retirement account; the text gives the date, the account, the bank's description and whether money was spent or received",
		Axes: []msg.ClassificationAxis{{
			Name:        categoryAxis,
			Description: "What the money was for. Choose the most specific category that fits; a parent category only when no child fits. Refunds of a purchase go under Refunds.",
			Values:      values,
			Required:    true,
		}},
	}
	if err := taxonomy.Validate(); err != nil {
		return msg.ClassificationTaxonomy{}, nil, err
	}
	return taxonomy, categoryByValue, nil
}

func itemText(c budgetstore.SuggestionCandidate) string {
	direction := fmt.Sprintf("spent $%d.%02d", c.AmountCents/100, c.AmountCents%100)
	if c.AmountCents < 0 {
		direction = fmt.Sprintf("received $%d.%02d", -c.AmountCents/100, (-c.AmountCents)%100)
	}
	text := fmt.Sprintf("%s | %s account %q | %s | %s", c.OccurredOn, c.AccountKind, c.AccountName, c.Description, direction)
	if c.PayeeName != "" {
		text += " | payee: " + c.PayeeName
	}
	return text
}

func (r runner) categorize(runKey string, batchNumber int, batch []budgetstore.SuggestionCandidate, taxonomy msg.ClassificationTaxonomy, categoryByValue map[string]string, maximumCost float64) (int, error) {
	items := make([]msg.ClassificationItem, len(batch))
	for i, c := range batch {
		items[i] = msg.ClassificationItem{ID: c.TransactionID, Text: itemText(c)}
	}
	input, err := json.Marshal(msg.ClassificationRunInput{Taxonomy: &taxonomy, Items: items, Model: r.modelRole})
	if err != nil {
		return 0, err
	}
	intent, err := json.Marshal(msg.OperationIntent{
		Type:           msg.OperationTypeClassificationRun,
		OrganizationID: r.organizationID,
		IdempotencyKey: fmt.Sprintf("%s-%s-%d", commandName, runKey, batchNumber),
		Input:          input,
		MaximumCostUSD: maximumCost,
	})
	if err != nil {
		return 0, err
	}
	var receipt msg.OperationReceipt
	if err := r.send(http.MethodPost, r.bridgeURL+"/operations", intent, &receipt, true); err != nil {
		return 0, fmt.Errorf("start operation: %w", err)
	}
	deadline := time.Now().Add(20 * time.Minute)
	for !receipt.State.IsTerminal() {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("operation %s still %s after 20 minutes", receipt.ID, receipt.State)
		}
		time.Sleep(5 * time.Second)
		if err := r.send(http.MethodGet, r.bridgeURL+"/operations/"+receipt.ID, nil, &receipt, true); err != nil {
			return 0, fmt.Errorf("read operation %s: %w", receipt.ID, err)
		}
	}
	if receipt.State != msg.OperationStateSucceeded {
		return 0, fmt.Errorf("operation %s ended %s: %s", receipt.ID, receipt.State, receiptError(receipt))
	}
	var result msg.ClassificationRunResult
	if err := json.Unmarshal(receipt.Result, &result); err != nil {
		return 0, fmt.Errorf("operation %s result: %w", receipt.ID, err)
	}
	asked := map[string]bool{}
	for _, c := range batch {
		asked[c.TransactionID] = true
	}
	suggestions := []budgetstore.SuggestionInput{}
	for _, item := range result.Items {
		if !asked[item.ID] {
			return 0, fmt.Errorf("operation %s answered for %q, which this batch did not ask about", receipt.ID, item.ID)
		}
		suggestion := budgetstore.SuggestionInput{TransactionID: item.ID, Confidence: item.Confidence, Rationale: item.Rationale, NeedsReview: item.NeedsReview}
		values := item.Values[categoryAxis]
		switch {
		case len(values) == 1 && values[0] != needsOperatorValue:
			categoryID, found := categoryByValue[values[0]]
			if !found {
				return 0, fmt.Errorf("operation %s gave %s the value %q, which this run did not offer", receipt.ID, item.ID, values[0])
			}
			suggestion.CategoryID = categoryID
		case len(values) == 1:
			// The model says the operator must decide: no category.
		default:
			suggestion.NeedsReview = true
		}
		suggestions = append(suggestions, suggestion)
	}
	body, err := json.Marshal(budgetstore.SuggestionBatch{OperationID: receipt.ID, Model: answeredBy(receipt), Suggestions: suggestions})
	if err != nil {
		return 0, err
	}
	var created map[string]int
	if err := r.send(http.MethodPost, r.budgetStoreURL+"/category-suggestions", body, &created, false); err != nil {
		return 0, fmt.Errorf("record suggestions from %s: %w", receipt.ID, err)
	}
	return created["created"], nil
}

// answeredBy names every model that answered, from the receipt's model_call
// evidence. A receipt with none is an error upstream; it is recorded as
// "unrecorded" rather than guessed.
func answeredBy(receipt msg.OperationReceipt) string {
	var models []string
	for _, evidence := range receipt.Evidence {
		if evidence.Kind != "model_call" {
			continue
		}
		var detail struct {
			AnsweredBy string `json:"answered_by"`
		}
		if json.Unmarshal(evidence.Detail, &detail) == nil && detail.AnsweredBy != "" && !slices.Contains(models, detail.AnsweredBy) {
			models = append(models, detail.AnsweredBy)
		}
	}
	if len(models) == 0 {
		log.Printf("operation %s has no model_call evidence naming a model", receipt.ID)
		return "unrecorded"
	}
	return strings.Join(models, ",")
}

func receiptError(receipt msg.OperationReceipt) string {
	if receipt.Error == nil {
		return "no error recorded"
	}
	text, _ := json.Marshal(receipt.Error)
	return string(text)
}

func (r runner) getJSON(url string, into any) error {
	return r.send(http.MethodGet, url, nil, into, false)
}

func (r runner) send(method, url string, body []byte, into any, toBridge bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if toBridge {
		request.Header.Set("Authorization", "Bearer "+r.bridgeToken)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s answered %s: %s", method, url, response.Status, strings.TrimSpace(string(answer)))
	}
	return json.Unmarshal(answer, into)
}
