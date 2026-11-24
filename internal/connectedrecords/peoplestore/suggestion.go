package peoplestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	var payload string
	if err := scan(&suggestion.ID, &suggestion.Source, &payload, &suggestion.Status, &suggestion.PersonID, &suggestion.CreatedAt, &suggestion.DecidedAt,
		&suggestion.Activity.LastActivityAt, &suggestion.Activity.MessageCount, &suggestion.Activity.DirectChat); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(payload), &suggestion.Payload); err != nil {
		return nil, fmt.Errorf("suggestion %s: payload: %w", suggestion.ID, err)
	}
	return &suggestion, nil
}

// suggestionSelect reads suggestions with their activity summed over their
// handles: the newest activity, the total messages, and whether any handle has
// a one-to-one chat. A handle no source reported adds nothing.
const suggestionSelect = `SELECT s.id, s.source, s.payload, s.status, s.person_id, s.created_at, s.decided_at,
	COALESCE(MAX(a.last_activity_at), 0), COALESCE(SUM(a.message_count), 0), COALESCE(MAX(a.direct_chat), 0)
	FROM suggestions s
	LEFT JOIN suggestion_handles si ON si.suggestion_id = s.id
	LEFT JOIN handle_activity a ON a.kind = si.kind AND a.value = si.value`

// suggestionOrders is the ORDER BY for each of SuggestionSorts.
var suggestionOrders = map[string]string{
	SuggestionSortCreated:  `s.seq`,
	SuggestionSortActivity: `COALESCE(MAX(a.direct_chat), 0) DESC, COALESCE(MAX(a.last_activity_at), 0) DESC, COALESCE(SUM(a.message_count), 0) DESC, s.seq`,
}

func getSuggestion(q queryer, id string) (*Suggestion, error) {
	suggestion, err := scanSuggestion(q.QueryRow(suggestionSelect+` WHERE s.id = ? GROUP BY s.id`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: suggestion %s", ErrNotFound, id)
	}
	return suggestion, err
}

// GetSuggestion returns one suggestion.
func (s *Store) GetSuggestion(id string) (*Suggestion, error) { return getSuggestion(s.db, id) }

// ListSuggestions returns suggestions in filter.Sort order, oldest first by
// default.
func (s *Store) ListSuggestions(filter SuggestionFilter) ([]Suggestion, error) {
	query := suggestionSelect
	var where []string
	var args []any
	if filter.Status != "" {
		if !contains(SuggestionStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: unknown status %q: use one of %s", ErrInvalid, filter.Status, strings.Join(SuggestionStatuses, ", "))
		}
		where = append(where, `s.status = ?`)
		args = append(args, filter.Status)
	}
	if text := strings.TrimSpace(filter.Query); text != "" {
		where = append(where, `instr(lower(json_extract(s.payload, '$.display_name')), lower(?)) > 0`)
		args = append(args, text)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
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
	query += fmt.Sprintf(` GROUP BY s.id ORDER BY %s LIMIT %d OFFSET %d`, order, limit, filter.Offset)
	rows, err := s.db.Query(query, args...)
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

// ProposeSuggestion records a source's guess that some handles are one
// person. Handles already attached to a person are left out, listed in
// Skipped, and their owners become matched_person_ids: the person accepting
// would extend. An importer source also leaves out every handle already in any
// suggestion whatever its status, so it never proposes a handle twice and a
// rejected guess is not proposed again. A reviewer source (agent) leaves out
// only handles in its own suggestions that still wait, must give a reason, and
// gets a 409 for a handle the operator refused for a matched person. When no
// handle is left, nothing is created.
func (s *Store) ProposeSuggestion(input SuggestionInput) (*SuggestionProposal, error) {
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

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	proposal := &SuggestionProposal{Skipped: []IdentityInput{}}
	var kept []IdentityInput
	matched := map[string]bool{}
	for _, identity := range identities {
		owner, err := identityOwner(tx, identity.Kind, identity.Value)
		if err != nil {
			return nil, err
		}
		if owner != nil {
			matched[owner.PersonID] = true
			proposal.Skipped = append(proposal.Skipped, identity)
			continue
		}
		var suggestedIn string
		if importer {
			err = tx.QueryRow(`SELECT suggestion_id FROM suggestion_handles WHERE kind = ? AND value = ? LIMIT 1`, identity.Kind, identity.Value).Scan(&suggestedIn)
		} else {
			err = tx.QueryRow(`SELECT s.id FROM suggestion_handles h JOIN suggestions s ON s.id = h.suggestion_id
				WHERE h.kind = ? AND h.value = ? AND s.source = ? AND s.status = ? LIMIT 1`, identity.Kind, identity.Value, source, SuggestionStatusProposed).Scan(&suggestedIn)
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
				refusal, err := refusalOf(tx, identity.Kind, identity.Value, personID)
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
	seq, err := nextSeq(tx, "suggestions")
	if err != nil {
		return nil, err
	}
	id := formatID(suggestionIDPrefix, seq)
	if _, err := tx.Exec(`INSERT INTO suggestions (id, seq, source, payload, status, created_at) VALUES (?,?,?,?,?,?)`,
		id, seq, source, string(payload), SuggestionStatusProposed, now()); err != nil {
		return nil, err
	}
	for _, identity := range kept {
		if _, err := tx.Exec(`INSERT INTO suggestion_handles (suggestion_id, kind, value) VALUES (?,?,?)`, id, identity.Kind, identity.Value); err != nil {
			return nil, err
		}
	}
	if proposal.Suggestion, err = getSuggestion(tx, id); err != nil {
		return nil, err
	}
	proposal.Created = true
	return proposal, tx.Commit()
}

// AcceptSuggestion attaches the suggestion's handles to personID, or to a new
// person named after the suggestion when personID is empty. Accepting is the
// operator's act, so each handle is linked by the operator, which overrides a
// refusal. It is all or nothing: a handle that has since gone to another
// person is a 409 and nothing is written.
func (s *Store) AcceptSuggestion(id, personID string) (*AcceptedSuggestion, error) {
	personID = strings.TrimSpace(personID)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	suggestion, err := getSuggestion(tx, id)
	if err != nil {
		return nil, err
	}
	if suggestion.Status != SuggestionStatusProposed {
		return nil, fmt.Errorf("%w: suggestion %s is already %s", ErrConflict, id, suggestion.Status)
	}
	if personID == "" {
		if personID, err = insertPerson(tx, suggestion.Payload.DisplayName, "", "", []string{}, ""); err != nil {
			return nil, err
		}
	} else if _, err := getLivePerson(tx, personID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: person_id: %v", ErrInvalid, err)
		}
		return nil, err
	}
	operator, err := linkSourceFor(LinkSourceOperator, "linked_by")
	if err != nil {
		return nil, err
	}
	reason := "accepted " + id
	if suggestion.Payload.Reason != "" {
		reason += ": " + suggestion.Payload.Reason
	}
	linkedAt := now()
	for _, identity := range suggestion.Payload.Identities {
		link := IdentityLinkInput{Kind: identity.Kind, Value: identity.Value, Label: identity.Label, LinkedBy: operator.Source, Reason: reason, LinkedAt: linkedAt}
		if _, _, err := addIdentity(tx, personID, link, operator, id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE suggestions SET status = ?, person_id = ?, decided_at = ? WHERE id = ?`,
		SuggestionStatusAccepted, personID, now(), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	accepted, err := s.GetSuggestion(id)
	if err != nil {
		return nil, err
	}
	person, err := s.GetPerson(personID)
	if err != nil {
		return nil, err
	}
	return &AcceptedSuggestion{Suggestion: *accepted, Person: *person}, nil
}

// RejectSuggestion marks a proposed suggestion rejected; rejecting is the
// operator's act. Its handles stay in suggestion_handles, so no importer
// proposes them again, and each is refused for every matched person, so no
// automatic source attaches it to them.
func (s *Store) RejectSuggestion(id string) (*Suggestion, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	suggestion, err := getSuggestion(tx, id)
	if err != nil {
		return nil, err
	}
	if suggestion.Status != SuggestionStatusProposed {
		return nil, fmt.Errorf("%w: suggestion %s is already %s", ErrConflict, id, suggestion.Status)
	}
	if _, err := tx.Exec(`UPDATE suggestions SET status = ?, decided_at = ? WHERE id = ?`, SuggestionStatusRejected, now(), id); err != nil {
		return nil, err
	}
	for _, identity := range suggestion.Payload.Identities {
		for _, personID := range suggestion.Payload.MatchedPersonIDs {
			if err := insertRefusal(tx, identity.Kind, identity.Value, personID, LinkSourceOperator, "rejected "+id); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetSuggestion(id)
}
