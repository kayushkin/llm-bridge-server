package peoplestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// SuggestionFilter selects suggestions. An empty Status lists every status;
// an empty Sort is SuggestionSortCreated. Query, when set, keeps suggestions
// whose display name holds it, ignoring ASCII case.
type SuggestionFilter struct {
	Status string
	Query  string
	Sort   string
	Limit  int
	Offset int
}

func scanSuggestion(scan func(...any) error) (*Suggestion, error) {
	var suggestion Suggestion
	var seq int64
	var personSeq sql.NullInt64
	var payload string
	if err := scan(&seq, &suggestion.OwnerPrincipalID, &suggestion.Source, &payload, &suggestion.Status, &personSeq, &suggestion.CreatedAt, &suggestion.DecidedAt,
		&suggestion.Activity.LastActivityAt, &suggestion.Activity.MessageCount, &suggestion.Activity.DirectChat); err != nil {
		return nil, err
	}
	suggestion.ID, suggestion.PersonID = formatID(suggestionIDPrefix, seq), formatOptionalPersonID(personSeq)
	if err := json.Unmarshal([]byte(payload), &suggestion.Payload); err != nil {
		return nil, fmt.Errorf("suggestion %s: payload: %w", suggestion.ID, err)
	}
	return &suggestion, nil
}

// suggestionSelect reads suggestions with their activity summed over their
// handles, as their owner's sources reported it: the newest activity, the
// total messages, and whether any handle has a one-to-one chat. A handle no
// source reported adds nothing.
const suggestionSelect = `SELECT s.seq, s.owner_principal_id, s.source, s.payload, s.status, s.person_seq, s.created_at, s.decided_at,
	COALESCE(MAX(a.last_activity_at), 0), COALESCE(SUM(a.message_count), 0)::bigint, COALESCE(bool_or(a.direct_chat), false)
	FROM suggestions s
	LEFT JOIN suggestion_handles si ON si.suggestion_seq = s.seq
	LEFT JOIN handle_activity a ON a.owner_principal_id = s.owner_principal_id AND a.kind = si.kind AND a.value = si.value`

// suggestionOrders is the ORDER BY for each of SuggestionSorts.
var suggestionOrders = map[string]string{
	SuggestionSortCreated:  `s.seq`,
	SuggestionSortActivity: `COALESCE(bool_or(a.direct_chat), false) DESC, COALESCE(MAX(a.last_activity_at), 0) DESC, COALESCE(SUM(a.message_count), 0) DESC, s.seq`,
}

func getSuggestion(ctx context.Context, q queryer, scope ownership.Scope, seq int64) (*Suggestion, error) {
	var args arguments
	query := suggestionSelect + ` WHERE s.seq = ` + args.add(seq) + ` AND ` + args.within(scope, "s.owner_principal_id") + ` GROUP BY s.seq`
	suggestion, err := scanSuggestion(q.QueryRowContext(ctx, query, args.values...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: suggestion %s", ErrNotFound, formatID(suggestionIDPrefix, seq))
	}
	return suggestion, err
}

// GetSuggestion returns one suggestion in scope.
func (s *Store) GetSuggestion(ctx context.Context, scope ownership.Scope, id string) (*Suggestion, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	seq, err := numberOf(suggestionIDPrefix, id)
	if err != nil {
		return nil, err
	}
	return getSuggestion(ctx, s.db, scope, seq)
}

// asciiLower lowers only A to Z, as SQLite's lower() did, so q ignores ASCII
// case and nothing else.
func asciiLower(expression string) string {
	return `translate(` + expression + `, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz')`
}

// ListSuggestions returns the suggestions in scope in filter.Sort order,
// oldest first by default.
func (s *Store) ListSuggestions(ctx context.Context, scope ownership.Scope, filter SuggestionFilter) ([]Suggestion, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	var args arguments
	where := []string{args.within(scope, "s.owner_principal_id")}
	if filter.Status != "" {
		if !contains(SuggestionStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s", ErrInvalid, filter.Status, strings.Join(SuggestionStatuses, ", "))
		}
		where = append(where, `s.status = `+args.add(filter.Status))
	}
	if text := strings.TrimSpace(filter.Query); text != "" {
		where = append(where, `strpos(`+asciiLower(`COALESCE(s.payload::jsonb ->> 'display_name', '')`)+`, `+asciiLower(args.add(text)+`::text`)+`) > 0`)
	}
	sortName := filter.Sort
	if sortName == "" {
		sortName = SuggestionSorts[0]
	}
	order, known := suggestionOrders[sortName]
	if !known {
		return nil, fmt.Errorf("%w: unknown sort %q: use one of %s", ErrInvalid, filter.Sort, strings.Join(SuggestionSorts, ", "))
	}
	if filter.Limit < 0 || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit and offset must not be negative", ErrInvalid)
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, fmt.Errorf("%w: limit %d is over 1000", ErrInvalid, limit)
	}
	query := suggestionSelect + ` WHERE ` + strings.Join(where, ` AND `) + fmt.Sprintf(` GROUP BY s.seq ORDER BY %s LIMIT %d OFFSET %d`, order, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suggestions := []Suggestion{}
	for rows.Next() {
		suggestion, err := scanSuggestion(rows.Scan)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, *suggestion)
	}
	return suggestions, rows.Err()
}

// ProposeSuggestion records a source's guess, kept by owner, that some handles
// are one person. Handles already attached to one of owner's persons are left
// out, listed in Skipped, and their persons become matched_person_ids: the
// person accepting would extend. An importer source also leaves out every
// handle already in any of owner's suggestions whatever its status, so it
// never proposes a handle twice and a rejected guess is not proposed again. A
// reviewer source (agent) leaves out only handles in its own suggestions that
// still wait, must give a reason, and gets a 409 for a handle the operator
// refused for a matched person. When no handle is left, nothing is created.
func (s *Store) ProposeSuggestion(ctx context.Context, owner string, input SuggestionInput) (*SuggestionProposal, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	source := strings.TrimSpace(input.Source)
	if !contains(SuggestionSources, source) {
		return nil, fmt.Errorf("%w: unknown source %q: use one of %s", ErrInvalid, input.Source, strings.Join(SuggestionSources, ", "))
	}
	importer := contains(importerSuggestionSources, source)
	reason := strings.TrimSpace(input.Reason)
	if !importer && reason == "" {
		return nil, fmt.Errorf("%w: reason is required from source %s: say why these handles are one person", ErrInvalid, source)
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		return nil, fmt.Errorf("%w: display_name is required: accepting the suggestion names the new person after it", ErrInvalid)
	}
	if len(input.Identities) == 0 {
		return nil, fmt.Errorf("%w: identities is required", ErrInvalid)
	}
	var identities []IdentityInput
	seen := map[string]bool{}
	for _, raw := range input.Identities {
		identity, err := NormalizeIdentity(raw)
		if err != nil {
			return nil, err
		}
		key := identity.Kind + "\x00" + identity.Value
		if !seen[key] {
			seen[key] = true
			identities = append(identities, identity)
		}
	}

	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	proposal := &SuggestionProposal{Skipped: []IdentityInput{}}
	var kept []IdentityInput
	matched := map[string]bool{}
	for _, identity := range identities {
		holder, err := identityHolder(ctx, tx, owner, identity.Kind, identity.Value)
		if err != nil {
			return nil, err
		}
		if holder != nil {
			matched[holder.PersonID] = true
			proposal.Skipped = append(proposal.Skipped, identity)
			continue
		}
		var suggestedIn int64
		if importer {
			err = tx.QueryRowContext(ctx, `SELECT suggestion_seq FROM suggestion_handles WHERE owner_principal_id = $1 AND kind = $2 AND value = $3 LIMIT 1`,
				owner, identity.Kind, identity.Value).Scan(&suggestedIn)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT s.seq FROM suggestion_handles h JOIN suggestions s ON s.seq = h.suggestion_seq
				WHERE h.owner_principal_id = $1 AND h.kind = $2 AND h.value = $3 AND s.source = $4 AND s.status = $5 LIMIT 1`,
				owner, identity.Kind, identity.Value, source, SuggestionStatusProposed).Scan(&suggestedIn)
		}
		if err == nil {
			proposal.Skipped = append(proposal.Skipped, identity)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		kept = append(kept, identity)
	}
	if len(kept) == 0 {
		return proposal, nil
	}
	matchedPersonIDs := []string{}
	for personID := range matched {
		matchedPersonIDs = append(matchedPersonIDs, personID)
	}
	sort.Strings(matchedPersonIDs)
	if !importer {
		for _, identity := range kept {
			for _, personID := range matchedPersonIDs {
				refusal, err := refusalOf(ctx, tx, identity.Kind, identity.Value, mustNumber(personIDPrefix, personID))
				if err != nil {
					return nil, err
				}
				if refusal != nil {
					return nil, fmt.Errorf("%w: %s %s was refused for %s by %s (%s, %s); only the operator can link them", ErrConflict, identity.Kind, identity.Value, personID, refusal.RefusedBy, refusal.ID, refusal.Reason)
				}
			}
		}
	}
	payload, err := json.Marshal(SuggestionPayload{DisplayName: displayName, Identities: kept, MatchedPersonIDs: matchedPersonIDs, SourceDetail: input.SourceDetail, Reason: reason})
	if err != nil {
		return nil, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO suggestions (owner_principal_id, source, payload, status, created_at) VALUES ($1, $2, $3, $4, $5) RETURNING seq`,
		owner, source, string(payload), SuggestionStatusProposed, now()).Scan(&seq); err != nil {
		return nil, err
	}
	for _, identity := range kept {
		if _, err := tx.ExecContext(ctx, `INSERT INTO suggestion_handles (owner_principal_id, suggestion_seq, kind, value) VALUES ($1, $2, $3, $4)`,
			owner, seq, identity.Kind, identity.Value); err != nil {
			return nil, err
		}
	}
	if proposal.Suggestion, err = getSuggestion(ctx, tx, ownership.OwnedBy(owner), seq); err != nil {
		return nil, err
	}
	proposal.Created = true
	return proposal, tx.Commit()
}

// beginForSuggestion starts a write on an existing suggestion in scope, holding
// its owner's lock.
func (s *Store) beginForSuggestion(ctx context.Context, scope ownership.Scope, id string) (*sql.Tx, int64, string, error) {
	if err := checkScope(scope); err != nil {
		return nil, 0, "", err
	}
	seq, err := numberOf(suggestionIDPrefix, id)
	if err != nil {
		return nil, 0, "", err
	}
	owner, err := ownerOfRow(ctx, s.db, scope, "suggestions", seq, suggestionIDPrefix)
	if err != nil {
		return nil, 0, "", err
	}
	tx, err := s.lockedTransaction(ctx, owner)
	if err != nil {
		return nil, 0, "", err
	}
	return tx, seq, owner, nil
}

// AcceptSuggestion attaches the suggestion's handles to personID, which must
// be a person of the suggestion's owner, or to a new person of that owner
// named after the suggestion when personID is empty. Accepting is the
// operator's act, so each handle is linked by the operator, which overrides a
// refusal. It is all or nothing: a handle that has since gone to another
// person is a 409 and nothing is written.
func (s *Store) AcceptSuggestion(ctx context.Context, scope ownership.Scope, id, personID string) (*AcceptedSuggestion, error) {
	personID = strings.TrimSpace(personID)
	tx, seq, owner, err := s.beginForSuggestion(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	suggestion, err := getSuggestion(ctx, tx, scope, seq)
	if err != nil {
		return nil, err
	}
	if suggestion.Status != SuggestionStatusProposed {
		return nil, fmt.Errorf("%w: suggestion %s is already %s", ErrConflict, id, suggestion.Status)
	}
	var personSeq int64
	if personID == "" {
		if personSeq, err = insertPerson(ctx, tx, owner, suggestion.Payload.DisplayName, "", "", []string{}, ""); err != nil {
			return nil, err
		}
	} else {
		person, err := otherLivePerson(ctx, tx, scope, owner, "person_id", personID)
		if err != nil {
			return nil, err
		}
		personSeq = mustNumber(personIDPrefix, person.ID)
	}
	operator, err := linkSourceFor(LinkSourceOperator, "linked_by")
	if err != nil {
		return nil, err
	}
	reason := "accepted " + suggestion.ID
	if suggestion.Payload.Reason != "" {
		reason += ": " + suggestion.Payload.Reason
	}
	linkedAt := now()
	for _, identity := range suggestion.Payload.Identities {
		link := IdentityLinkInput{Kind: identity.Kind, Value: identity.Value, Label: identity.Label, LinkedBy: operator.Source, Reason: reason, LinkedAt: linkedAt}
		if _, _, err := addIdentity(ctx, tx, owner, personSeq, link, operator, seq); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET status = $1, person_seq = $2, decided_at = $3 WHERE seq = $4`,
		SuggestionStatusAccepted, personSeq, now(), seq); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ownerScope := ownership.OwnedBy(owner)
	accepted, err := getSuggestion(ctx, s.db, ownerScope, seq)
	if err != nil {
		return nil, err
	}
	person, err := personDetail(ctx, s.db, ownerScope, personSeq)
	if err != nil {
		return nil, err
	}
	return &AcceptedSuggestion{Suggestion: *accepted, Person: *person}, nil
}

// RejectSuggestion marks a proposed suggestion rejected; rejecting is the
// operator's act. Its handles stay in suggestion_handles, so no importer
// proposes them again, and each is refused for every matched person, so no
// automatic source attaches it to them.
func (s *Store) RejectSuggestion(ctx context.Context, scope ownership.Scope, id string) (*Suggestion, error) {
	tx, seq, owner, err := s.beginForSuggestion(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	suggestion, err := getSuggestion(ctx, tx, scope, seq)
	if err != nil {
		return nil, err
	}
	if suggestion.Status != SuggestionStatusProposed {
		return nil, fmt.Errorf("%w: suggestion %s is already %s", ErrConflict, id, suggestion.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET status = $1, decided_at = $2 WHERE seq = $3`, SuggestionStatusRejected, now(), seq); err != nil {
		return nil, err
	}
	for _, identity := range suggestion.Payload.Identities {
		for _, personID := range suggestion.Payload.MatchedPersonIDs {
			personSeq, err := numberOf(personIDPrefix, personID)
			if err != nil {
				return nil, fmt.Errorf("suggestion %s matches %q, which is not a person id: %w", suggestion.ID, personID, err)
			}
			if err := insertRefusal(ctx, tx, owner, identity.Kind, identity.Value, personSeq, LinkSourceOperator, "rejected "+suggestion.ID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return getSuggestion(ctx, s.db, ownership.OwnedBy(owner), seq)
}
