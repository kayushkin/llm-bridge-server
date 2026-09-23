package operationstore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Spending is kept twice on purpose, for two readers: each receipt carries
// its own usage, and the operation_spend ledger holds one row per model call
// so an organization's month and an operation tree's total can be summed
// without reading every receipt. RecordModelCall writes both in one
// transaction, so they cannot disagree.

const spendSchema = `
CREATE TABLE IF NOT EXISTS operation_spend (
	operation_id      TEXT NOT NULL REFERENCES operations(id),
	root_operation_id TEXT NOT NULL,
	organization_id   TEXT NOT NULL,
	model             TEXT NOT NULL,
	input_tokens      INTEGER NOT NULL,
	output_tokens     INTEGER NOT NULL,
	cache_read_tokens INTEGER NOT NULL,
	cache_write_tokens INTEGER NOT NULL,
	cost_usd          REAL NOT NULL,
	cost_known        INTEGER NOT NULL,
	at                INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS operation_spend_organization_at ON operation_spend(organization_id, at);
CREATE INDEX IF NOT EXISTS operation_spend_root ON operation_spend(root_operation_id);
CREATE TABLE IF NOT EXISTS organization_budgets (
	organization_id   TEXT PRIMARY KEY,
	monthly_limit_usd REAL NOT NULL,
	updated_by        TEXT NOT NULL,
	updated_at        INTEGER NOT NULL
);
`

// ModelCall is one model call's usage, and its list-price cost when known.
type ModelCall struct {
	Model   string
	Tokens  msg.TokenUsage
	CostUSD float64
	// CostKnown is false when model-store has no price for Model; CostUSD is
	// then 0 and says nothing.
	CostKnown bool
}

// RecordModelCall adds a call to the operation's usage and to the ledger.
func (s *Store) RecordModelCall(operationID string, leaseToken int64, now time.Time, call ModelCall) (msg.OperationReceipt, error) {
	return s.update(operationID, leaseToken, now, func(operation *Operation) (msg.OperationEventKind, error) {
		usage := operation.Receipt.Usage
		if usage == nil {
			usage = &msg.OperationUsage{CostBasis: msg.OperationCostBasisListPrice}
			operation.Receipt.Usage = usage
		}
		usage.Calls++
		usage.Tokens.InputTokens += call.Tokens.InputTokens
		usage.Tokens.OutputTokens += call.Tokens.OutputTokens
		usage.Tokens.TotalTokens += call.Tokens.InputTokens + call.Tokens.OutputTokens
		usage.Tokens.CacheReadTokens += call.Tokens.CacheReadTokens
		usage.Tokens.CacheWriteTokens += call.Tokens.CacheWriteTokens
		usage.Cost.TotalUSD += call.CostUSD
		return msg.OperationEventProgress, nil
	}, func(tx *sql.Tx, operation Operation) error {
		var rootOperationID string
		if err := tx.QueryRow(`SELECT root_operation_id FROM operations WHERE id = ?`, operationID).Scan(&rootOperationID); err != nil {
			return fmt.Errorf("read root of %s: %w", operationID, err)
		}
		_, err := tx.Exec(`INSERT INTO operation_spend (operation_id, root_operation_id, organization_id, model, input_tokens,
			output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, cost_known, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			operationID, rootOperationID, operation.Receipt.OrganizationID, call.Model, call.Tokens.InputTokens, call.Tokens.OutputTokens,
			call.Tokens.CacheReadTokens, call.Tokens.CacheWriteTokens, call.CostUSD, call.CostKnown, now.UnixNano())
		if err != nil {
			return fmt.Errorf("record spend of %s: %w", operationID, err)
		}
		return nil
	})
}

// MonthStart is the start of the UTC calendar month holding at.
func MonthStart(at time.Time) time.Time {
	utc := at.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// OrganizationSpentUSD sums an organization's list-price spend since from.
func (s *Store) OrganizationSpentUSD(organizationID string, from time.Time) (float64, error) {
	var spent float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM operation_spend WHERE organization_id = ? AND at >= ?`,
		organizationID, from.UnixNano()).Scan(&spent)
	if err != nil {
		return 0, fmt.Errorf("sum spend of %s: %w", organizationID, err)
	}
	return spent, nil
}

// TreeSpentUSD sums the spend of an operation and every descendant.
func (s *Store) TreeSpentUSD(rootOperationID string) (float64, error) {
	var spent float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM operation_spend WHERE root_operation_id = ?`, rootOperationID).Scan(&spent)
	if err != nil {
		return 0, fmt.Errorf("sum spend under %s: %w", rootOperationID, err)
	}
	return spent, nil
}

// RootOf returns the root operation of operationID and that root's intent,
// which carries the per-operation cost cap.
func (s *Store) RootOf(operationID string) (Operation, error) {
	var rootOperationID string
	if err := s.db.QueryRow(`SELECT root_operation_id FROM operations WHERE id = ?`, operationID).Scan(&rootOperationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, operationID)
		}
		return Operation{}, err
	}
	return s.Get(rootOperationID)
}

// SetOrganizationBudget sets an organization's monthly limit.
func (s *Store) SetOrganizationBudget(organizationID string, monthlyLimitUSD float64, updatedBy string, now time.Time) error {
	if monthlyLimitUSD < 0 {
		return fmt.Errorf("monthly limit must not be negative, got %v", monthlyLimitUSD)
	}
	_, err := s.db.Exec(`INSERT INTO organization_budgets (organization_id, monthly_limit_usd, updated_by, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(organization_id) DO UPDATE SET monthly_limit_usd = excluded.monthly_limit_usd,
		updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		organizationID, monthlyLimitUSD, updatedBy, now.UnixNano())
	if err != nil {
		return fmt.Errorf("set budget of %s: %w", organizationID, err)
	}
	return nil
}

// DeleteOrganizationBudget removes an organization's limit; ErrNotFound when
// it had none.
func (s *Store) DeleteOrganizationBudget(organizationID string) error {
	result, err := s.db.Exec(`DELETE FROM organization_budgets WHERE organization_id = ?`, organizationID)
	if err != nil {
		return fmt.Errorf("delete budget of %s: %w", organizationID, err)
	}
	if deleted, _ := result.RowsAffected(); deleted == 0 {
		return fmt.Errorf("%w: %s has no budget", ErrNotFound, organizationID)
	}
	return nil
}

// OrganizationBudget returns an organization's limit and this month's spend.
// found is false when it has no limit.
func (s *Store) OrganizationBudget(organizationID string, now time.Time) (budget msg.OrganizationBudget, found bool, err error) {
	var updatedAt int64
	err = s.db.QueryRow(`SELECT monthly_limit_usd, updated_by, updated_at FROM organization_budgets WHERE organization_id = ?`, organizationID).
		Scan(&budget.MonthlyLimitUSD, &budget.UpdatedBy, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return msg.OrganizationBudget{}, false, nil
	}
	if err != nil {
		return msg.OrganizationBudget{}, false, fmt.Errorf("read budget of %s: %w", organizationID, err)
	}
	budget.OrganizationID = organizationID
	budget.UpdatedAt = time.Unix(0, updatedAt).UTC()
	budget.MonthStartsAt = MonthStart(now)
	budget.SpentUSD, err = s.OrganizationSpentUSD(organizationID, budget.MonthStartsAt)
	return budget, err == nil, err
}

// ListOrganizationBudgets returns every organization with a limit.
func (s *Store) ListOrganizationBudgets(now time.Time) ([]msg.OrganizationBudget, error) {
	rows, err := s.db.Query(`SELECT organization_id FROM organization_budgets ORDER BY organization_id`)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	var organizationIDs []string
	for rows.Next() {
		var organizationID string
		if err := rows.Scan(&organizationID); err != nil {
			rows.Close()
			return nil, err
		}
		organizationIDs = append(organizationIDs, organizationID)
	}
	rows.Close()
	budgets := []msg.OrganizationBudget{}
	for _, organizationID := range organizationIDs {
		budget, found, err := s.OrganizationBudget(organizationID, now)
		if err != nil {
			return nil, err
		}
		if found {
			budgets = append(budgets, budget)
		}
	}
	return budgets, nil
}
