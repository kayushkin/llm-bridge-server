package rulestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Rule states. A new rule is a draft; activating a revision makes it active;
// pause stops it deciding; retire ends it for good. Nothing is deleted.
const (
	RuleStateDraft   = "draft"
	RuleStateActive  = "active"
	RuleStatePaused  = "paused"
	RuleStateRetired = "retired"
)

// RuleStates is every state a rule can be in, served at GET /vocabulary.
var RuleStates = []string{RuleStateDraft, RuleStateActive, RuleStatePaused, RuleStateRetired}

// Rule is a rules row, plus its revisions and activations on GET /rules/{id}.
type Rule struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Description      string           `json:"description"`
	Source           string           `json:"source"`
	State            string           `json:"state"`
	ActiveRevisionID string           `json:"active_revision_id"`
	Mode             string           `json:"mode"`
	CreatedBy        string           `json:"created_by"`
	CreatedAt        int64            `json:"created_at"`
	UpdatedAt        int64            `json:"updated_at"`
	Revisions        []RuleRevision   `json:"revisions,omitempty"`
	Activations      []RuleActivation `json:"activations,omitempty"`
}

// RuleRevision is one immutable version of a rule's definition.
type RuleRevision struct {
	ID         string     `json:"id"`
	RuleID     string     `json:"rule_id"`
	Revision   int        `json:"revision"`
	Definition Definition `json:"definition"`
	Note       string     `json:"note"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  int64      `json:"created_at"`
}

// RuleActivation is one activate, pause or retire.
type RuleActivation struct {
	ID         string `json:"id"`
	RuleID     string `json:"rule_id"`
	Action     string `json:"action"`
	RevisionID string `json:"revision_id"`
	Mode       string `json:"mode"`
	ActedBy    string `json:"acted_by"`
	Reason     string `json:"reason"`
	ActedAt    int64  `json:"acted_at"`
}

// principalIDPattern is principal-store's id shape. rule-store has no auth
// and cannot check the principal exists; it can refuse a name or an email
// where an id belongs.
var principalIDPattern = regexp.MustCompile(`^principal_[0-9]{6,}$`)

func requirePrincipal(id, field string) error {
	if !principalIDPattern.MatchString(id) {
		return invalidf("%s %q must be a principal-store id such as principal_000001", field, id)
	}
	return nil
}

const ruleColumns = `id, name, description, source, state, active_revision_id, mode, created_by, created_at, updated_at`

func scanRule(scan func(...any) error) (*Rule, error) {
	r := &Rule{}
	return r, scan(&r.ID, &r.Name, &r.Description, &r.Source, &r.State, &r.ActiveRevisionID, &r.Mode, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
}

// CreateRule makes a draft rule with no revision.
func (s *Store) CreateRule(name, description, source, createdBy string) (*Rule, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, invalidf("name is required")
	}
	if err := oneOf(source, Sources, "source"); err != nil {
		return nil, err
	}
	if err := requirePrincipal(createdBy, "created_by"); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "rules")
	if err != nil {
		return nil, err
	}
	at := now()
	r := &Rule{ID: formatID("rule", seq), Name: name, Description: strings.TrimSpace(description), Source: source,
		State: RuleStateDraft, CreatedBy: createdBy, CreatedAt: at, UpdatedAt: at}
	if _, err := tx.Exec(`INSERT INTO rules (id, seq, name, description, source, state, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		r.ID, seq, r.Name, r.Description, r.Source, r.State, r.CreatedBy, r.CreatedAt, r.UpdatedAt); err != nil {
		return nil, err
	}
	return r, tx.Commit()
}

// GetRule returns a rule with its revisions and activations.
func (s *Store) GetRule(id string) (*Rule, error) {
	r, err := scanRule(s.db.QueryRow(`SELECT `+ruleColumns+` FROM rules WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: rule %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if r.Revisions, err = s.revisionsOf(id); err != nil {
		return nil, err
	}
	if r.Activations, err = s.activationsOf(id); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRules returns every rule, oldest first, without revisions. state
// filters when set.
func (s *Store) ListRules(state string) ([]*Rule, error) {
	query, args := `SELECT `+ruleColumns+` FROM rules`, []any{}
	if state != "" {
		if err := oneOf(state, RuleStates, "rule state"); err != nil {
			return nil, err
		}
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	rows, err := s.db.Query(query+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []*Rule{}
	for rows.Next() {
		r, err := scanRule(rows.Scan)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// AddRevision validates a definition and stores it as the rule's next
// revision. It does not activate it.
func (s *Store) AddRevision(ruleID string, raw json.RawMessage, note, createdBy string) (*RuleRevision, error) {
	if err := requirePrincipal(createdBy, "created_by"); err != nil {
		return nil, err
	}
	rule, err := scanRule(s.db.QueryRow(`SELECT `+ruleColumns+` FROM rules WHERE id = ?`, ruleID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: rule %s", ErrNotFound, ruleID)
	}
	if err != nil {
		return nil, err
	}
	if rule.State == RuleStateRetired {
		return nil, fmt.Errorf("%w: rule %s is retired; make a new rule", ErrConflict, ruleID)
	}
	definition, err := ParseDefinition(rule.Source, raw)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(definition)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "rule_revisions")
	if err != nil {
		return nil, err
	}
	var latest sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(revision) FROM rule_revisions WHERE rule_id = ?`, ruleID).Scan(&latest); err != nil {
		return nil, err
	}
	rev := &RuleRevision{ID: formatID("rule_revision", seq), RuleID: ruleID, Revision: int(latest.Int64) + 1,
		Definition: definition, Note: strings.TrimSpace(note), CreatedBy: createdBy, CreatedAt: now()}
	if _, err := tx.Exec(`INSERT INTO rule_revisions (id, seq, rule_id, revision, definition, note, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		rev.ID, seq, rev.RuleID, rev.Revision, string(canonical), rev.Note, rev.CreatedBy, rev.CreatedAt); err != nil {
		return nil, err
	}
	return rev, tx.Commit()
}

// GetRevision returns one revision.
func (s *Store) GetRevision(id string) (*RuleRevision, error) {
	rev, err := scanRevision(s.db.QueryRow(`SELECT id, rule_id, revision, definition, note, created_by, created_at FROM rule_revisions WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: rule revision %s", ErrNotFound, id)
	}
	return rev, err
}

func scanRevision(scan func(...any) error) (*RuleRevision, error) {
	rev := &RuleRevision{}
	var definition string
	if err := scan(&rev.ID, &rev.RuleID, &rev.Revision, &definition, &rev.Note, &rev.CreatedBy, &rev.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(definition), &rev.Definition); err != nil {
		return nil, fmt.Errorf("revision %s holds an unreadable definition: %w", rev.ID, err)
	}
	return rev, nil
}

func (s *Store) revisionsOf(ruleID string) ([]RuleRevision, error) {
	rows, err := s.db.Query(`SELECT id, rule_id, revision, definition, note, created_by, created_at FROM rule_revisions WHERE rule_id = ? ORDER BY revision`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []RuleRevision{}
	for rows.Next() {
		rev, err := scanRevision(rows.Scan)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, *rev)
	}
	return revisions, rows.Err()
}

func (s *Store) activationsOf(ruleID string) ([]RuleActivation, error) {
	rows, err := s.db.Query(`SELECT id, rule_id, action, revision_id, mode, acted_by, reason, acted_at FROM rule_activations WHERE rule_id = ? ORDER BY seq`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activations := []RuleActivation{}
	for rows.Next() {
		var a RuleActivation
		if err := rows.Scan(&a.ID, &a.RuleID, &a.Action, &a.RevisionID, &a.Mode, &a.ActedBy, &a.Reason, &a.ActedAt); err != nil {
			return nil, err
		}
		activations = append(activations, a)
	}
	return activations, rows.Err()
}

// Activate puts a revision of the rule in force, in a mode. It also switches
// an active rule to another revision. A reason is required: the next person
// to read the log needs to know why this text started deciding.
func (s *Store) Activate(ruleID string, revisionNumber int, mode, actedBy, reason string) (*Rule, error) {
	if err := oneOf(mode, Modes, "mode"); err != nil {
		return nil, err
	}
	var revisionID string
	err := s.db.QueryRow(`SELECT id FROM rule_revisions WHERE rule_id = ? AND revision = ?`, ruleID, revisionNumber).Scan(&revisionID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, getErr := s.GetRule(ruleID); getErr != nil {
			return nil, getErr
		}
		return nil, invalidf("rule %s has no revision %d", ruleID, revisionNumber)
	}
	if err != nil {
		return nil, err
	}
	return s.act(ruleID, "activate", RuleStateActive, revisionID, mode, actedBy, reason,
		[]string{RuleStateDraft, RuleStateActive, RuleStatePaused})
}

// Pause stops an active rule deciding, keeping its revision and mode.
func (s *Store) Pause(ruleID, actedBy, reason string) (*Rule, error) {
	return s.act(ruleID, "pause", RuleStatePaused, "", "", actedBy, reason, []string{RuleStateActive})
}

// Retire ends a rule. A retired rule takes no revision and cannot come back.
func (s *Store) Retire(ruleID, actedBy, reason string) (*Rule, error) {
	return s.act(ruleID, "retire", RuleStateRetired, "", "", actedBy, reason,
		[]string{RuleStateDraft, RuleStateActive, RuleStatePaused})
}

func (s *Store) act(ruleID, action, toState, revisionID, mode, actedBy, reason string, fromStates []string) (*Rule, error) {
	if err := requirePrincipal(actedBy, "acted_by"); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, invalidf("reason is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rule, err := scanRule(tx.QueryRow(`SELECT `+ruleColumns+` FROM rules WHERE id = ?`, ruleID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: rule %s", ErrNotFound, ruleID)
	}
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, from := range fromStates {
		allowed = allowed || rule.State == from
	}
	if !allowed {
		return nil, fmt.Errorf("%w: rule %s is %s; %s needs it to be %s", ErrConflict, ruleID, rule.State, action, strings.Join(fromStates, " or "))
	}
	if revisionID == "" {
		revisionID, mode = rule.ActiveRevisionID, rule.Mode
	}
	seq, err := nextSeq(tx, "rule_activations")
	if err != nil {
		return nil, err
	}
	at := now()
	if _, err := tx.Exec(`INSERT INTO rule_activations (id, seq, rule_id, action, revision_id, mode, acted_by, reason, acted_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		formatID("rule_activation", seq), seq, ruleID, action, revisionID, mode, actedBy, reason, at); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE rules SET state = ?, active_revision_id = ?, mode = ?, updated_at = ? WHERE id = ?`,
		toState, revisionID, mode, at, ruleID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetRule(ruleID)
}

// ActiveRule is an active rule with its revision loaded, for the reactor.
type ActiveRule struct {
	Rule     Rule
	Revision RuleRevision
}

// ActiveRules returns every active rule watching a source.
func (s *Store) ActiveRules(source string) ([]ActiveRule, error) {
	rules, err := s.ListRules(RuleStateActive)
	if err != nil {
		return nil, err
	}
	active := []ActiveRule{}
	for _, r := range rules {
		if r.Source != source {
			continue
		}
		rev, err := s.GetRevision(r.ActiveRevisionID)
		if err != nil {
			return nil, fmt.Errorf("active rule %s names revision %s: %w", r.ID, r.ActiveRevisionID, err)
		}
		active = append(active, ActiveRule{Rule: *r, Revision: *rev})
	}
	return active, nil
}
