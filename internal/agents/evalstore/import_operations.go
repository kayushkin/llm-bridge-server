package evalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"

	"github.com/kayushkin/llm-bridge/msg"
)

// OperationSource reads operations from llm-bridge-server.
type OperationSource interface {
	ListOperations(ctx context.Context, filter OperationsFilter) ([]msg.OperationReceipt, error)
	ReadIntent(ctx context.Context, id string) (msg.OperationIntent, error)
}

// ImportOperationsRequest is what POST /suites/{id}/cases/import-operations
// takes: which operations, and how many of them.
type ImportOperationsRequest struct {
	OperationsFilter
	// SampleSize, when positive, takes that many of the matching operations
	// not yet imported, chosen by a shuffle seeded by Seed.
	SampleSize int   `json:"sample_size"`
	Seed       int64 `json:"seed"`
}

// ImportOperationsResult says what the import did.
type ImportOperationsResult struct {
	Matched         int      `json:"matched"`
	AlreadyImported int      `json:"already_imported"`
	Imported        []string `json:"imported"`
	// SchemaDiffers lists imported cases whose call used a schema other than
	// the suite's output schema: replays of them use the suite's.
	SchemaDiffers []string `json:"schema_differs"`
}

// ImportOperations copies succeeded llm.completion operations into a suite
// as cases. Each case keeps the prompts, the answer and its usage, and names
// the operation by its id.
func (s *Store) ImportOperations(ctx context.Context, suiteID string, request ImportOperationsRequest, source OperationSource) (*ImportOperationsResult, error) {
	suite, err := s.GetSuite(suiteID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, invalidf("EVAL_STORE_LLM_BRIDGE_URL is not set, so there are no operations to import")
	}
	if request.Type == "" {
		request.Type = string(msg.OperationTypeLLMCompletion)
	}
	if request.Type != string(msg.OperationTypeLLMCompletion) {
		return nil, invalidf("only %s operations make cases; %s does not carry one prompt and one answer", msg.OperationTypeLLMCompletion, request.Type)
	}
	if request.SampleSize < 0 {
		return nil, invalidf("sample_size may not be negative")
	}
	receipts, err := source.ListOperations(ctx, request.OperationsFilter)
	if err != nil {
		return nil, err
	}
	result := &ImportOperationsResult{Matched: len(receipts), Imported: []string{}, SchemaDiffers: []string{}}
	existing := map[string]bool{}
	cases, err := s.queryCases("SELECT "+caseSummaryColumns+" FROM cases WHERE suite_id = ? AND source_kind = ?", false, suite.ID, SourceKindLLMBridgeOperation)
	if err != nil {
		return nil, err
	}
	for _, c := range cases {
		existing[c.SourceRef] = true
	}
	var fresh []msg.OperationReceipt
	for _, receipt := range receipts {
		if existing[receipt.ID] {
			result.AlreadyImported++
			continue
		}
		fresh = append(fresh, receipt)
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].ID < fresh[j].ID })
	if request.SampleSize > 0 && request.SampleSize < len(fresh) {
		random := rand.New(rand.NewPCG(uint64(request.Seed), 0x696d706f7274))
		random.Shuffle(len(fresh), func(i, j int) { fresh[i], fresh[j] = fresh[j], fresh[i] })
		fresh = fresh[:request.SampleSize]
		sort.Slice(fresh, func(i, j int) bool { return fresh[i].ID < fresh[j].ID })
	}

	var suiteSchema any
	json.Unmarshal(suite.OutputSchema, &suiteSchema)
	newCases := make([]*Case, 0, len(fresh))
	for _, receipt := range fresh {
		intent, err := source.ReadIntent(ctx, receipt.ID)
		if err != nil {
			return nil, err
		}
		c, schemaDiffers, err := caseFromOperation(receipt, intent, suiteSchema)
		if err != nil {
			return nil, err
		}
		if schemaDiffers {
			result.SchemaDiffers = append(result.SchemaDiffers, receipt.ID)
		}
		newCases = append(newCases, c)
	}
	err = s.inTransaction(func(tx *sql.Tx) error {
		for _, c := range newCases {
			id, err := insertCase(tx, suite, c, s.unixNow())
			if err != nil {
				return err
			}
			result.Imported = append(result.Imported, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func caseFromOperation(receipt msg.OperationReceipt, intent msg.OperationIntent, suiteSchema any) (*Case, bool, error) {
	var input msg.LLMCompletionInput
	if err := json.Unmarshal(intent.Input, &input); err != nil {
		return nil, false, fmt.Errorf("operation %s: input is not an llm.completion input: %w", receipt.ID, err)
	}
	outcome, err := outcomeOfReceipt(receipt)
	if err != nil {
		return nil, false, fmt.Errorf("operation %s: %w", receipt.ID, err)
	}
	var callSchema any
	json.Unmarshal(input.Schema, &callSchema)
	c := &Case{SourceKind: SourceKindLLMBridgeOperation, SourceRef: receipt.ID, SystemPrompt: input.SystemPrompt, UserPrompt: input.Prompt,
		OriginalModelID: outcome.AnsweredBy, OriginalAnswer: outcome.Parsed, OriginalUsageKnown: outcome.UsageKnown,
		OriginalInputTokens: outcome.InputTokens, OriginalCacheReadTokens: outcome.CacheReadTokens, OriginalCacheWriteTokens: outcome.CacheWriteTokens,
		OriginalOutputTokens: outcome.OutputTokens, OriginalDurationMs: outcome.DurationMs, OriginalAt: receipt.CreatedAt.Unix()}
	return c, !reflect.DeepEqual(callSchema, suiteSchema), nil
}
