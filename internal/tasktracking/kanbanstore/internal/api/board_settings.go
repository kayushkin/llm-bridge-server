package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// Board settings — the defaults a dispatcher or classifier reads off the board
// instead of taking as a flag on a cron job. Three of them are ids another
// store hands out, and each is asked before the write: a 400 when the owner
// says the id does not exist, a 502 when the owner could not be asked, and
// nothing written either way. The reasoning is the one card assignments
// follow (internal/principalstore): a wrong default here is not a dangling
// pointer a reader can notice, it is a board whose every dispatch fails in a
// cron log, far from the PATCH that caused it.

// settingsCheckFailure carries the status the owner's answer maps to, so the
// PATCH handler reports a wrong id (400) and an owner that is down (502)
// differently — they mean opposite things to the caller.
type settingsCheckFailure struct {
	status  int
	message string
}

func (f *settingsCheckFailure) Error() string { return f.message }

// checkBoardSettings asks each owner about every non-empty id in the request.
// An empty string is a clear and needs no owner.
func (a *API) checkBoardSettings(req *model.UpdateBoardRequest) error {
	if req.DefaultPrincipalID != nil && *req.DefaultPrincipalID != "" {
		if err := a.checkPrincipalIsAssignable(*req.DefaultPrincipalID); err != nil {
			return err
		}
	}
	if req.Classifier != nil && req.Classifier.OrganizationID != "" {
		if err := a.checkPrincipalIsActiveGroup("classifier.organization_id", req.Classifier.OrganizationID); err != nil {
			return err
		}
	}
	if req.OrganizationID != nil && *req.OrganizationID != "" {
		if err := a.checkPrincipalIsActiveGroup("organization_id", *req.OrganizationID); err != nil {
			return err
		}
	}
	if req.AssignmentPool != nil && !req.AssignmentPool.Cleared() {
		if err := a.checkPrincipalIsActiveGroup("assignment_pool.principal_id", req.AssignmentPool.PrincipalID); err != nil {
			return err
		}
	}
	if req.DefaultAgentID != nil && *req.DefaultAgentID != "" {
		if err := a.bridge.CheckAgentExists(*req.DefaultAgentID); err != nil {
			if errors.Is(err, llmbridge.ErrNotFound) {
				return &settingsCheckFailure{400, "default_agent_id: " + err.Error()}
			}
			return &settingsCheckFailure{502, "llm-bridge-server check of default_agent_id failed: " + err.Error()}
		}
	}
	if req.DefaultInstanceID != nil && *req.DefaultInstanceID != "" {
		if err := a.bridge.CheckInstanceExists(*req.DefaultInstanceID); err != nil {
			if errors.Is(err, llmbridge.ErrNotFound) {
				return &settingsCheckFailure{400, "default_instance_id: " + err.Error()}
			}
			return &settingsCheckFailure{502, "llm-bridge-server check of default_instance_id failed: " + err.Error()}
		}
	}
	if req.DefaultBundleID != nil && *req.DefaultBundleID != "" {
		if err := a.bundles.CheckBundleExists(*req.DefaultBundleID); err != nil {
			if errors.Is(err, bundlestore.ErrNotFound) {
				return &settingsCheckFailure{400, "default_bundle_id: " + err.Error()}
			}
			return &settingsCheckFailure{502, "bundle-store check of default_bundle_id failed: " + err.Error()}
		}
	}
	return nil
}

// checkPrincipalIsAssignable is the same question PUT /assignments asks, with
// the same answers: malformed or unknown or disabled is 400, unreachable is 502.
func (a *API) checkPrincipalIsAssignable(principalID string) error {
	if !principalIDShape.MatchString(principalID) {
		return &settingsCheckFailure{400, fmt.Sprintf("default_principal_id must match ^%s$ (for example principal_000001), got %q", config.PrincipalIDPattern, boundedtext.Value(principalID))}
	}
	principal, err := a.principals.Get(principalID)
	if errors.Is(err, principalstore.ErrNotFound) {
		return &settingsCheckFailure{400, fmt.Sprintf("default_principal_id %s does not exist in principal-store", boundedtext.Value(principalID))}
	}
	if err != nil {
		return &settingsCheckFailure{502, "principal-store check failed: " + err.Error()}
	}
	if principal.Disabled() {
		return &settingsCheckFailure{400, fmt.Sprintf("default_principal_id %s is disabled in principal-store", boundedtext.Value(principalID))}
	}
	return nil
}

// checkPrincipalIsActiveGroup asks principal-store whether id is a group
// that is not disabled. A classifier's organization is a group, and a person
// or a contact in its place would make every operation run as the wrong
// thing; an assignment pool is a group, and a person in its place has no
// members to choose among.
func (a *API) checkPrincipalIsActiveGroup(field, principalID string) error {
	if !principalIDShape.MatchString(principalID) {
		return &settingsCheckFailure{400, fmt.Sprintf("%s must match ^%s$ (for example principal_000006), got %q", field, config.PrincipalIDPattern, boundedtext.Value(principalID))}
	}
	principal, err := a.principals.Get(principalID)
	if errors.Is(err, principalstore.ErrNotFound) {
		return &settingsCheckFailure{400, fmt.Sprintf("%s %s does not exist in principal-store", field, boundedtext.Value(principalID))}
	}
	if err != nil {
		return &settingsCheckFailure{502, "principal-store check failed: " + err.Error()}
	}
	if principal.Kind != "group" {
		return &settingsCheckFailure{400, fmt.Sprintf("%s %s is a %s; it must be a principal-store group", field, principalID, principal.Kind)}
	}
	if principal.Disabled() {
		return &settingsCheckFailure{400, fmt.Sprintf("%s %s is disabled in principal-store", field, boundedtext.Value(principalID))}
	}
	return nil
}

func writeSettingsCheckFailure(w http.ResponseWriter, err error) {
	var failure *settingsCheckFailure
	if errors.As(err, &failure) {
		writeError(w, failure.status, failure.message)
		return
	}
	writeError(w, 500, err.Error())
}

// arrivalAssignment is what happens to the assignee of a card arriving on a
// board with nobody on it: either a principal is put on it, or — when the
// board's assignment pool had nobody available and the board has no default —
// it is left unassigned and the log says why. Detail is the event's detail in
// either case.
type arrivalAssignment struct {
	principalID string
	skipped     bool
	detail      map[string]any
}

// assigneeOnArrival answers what happens to the assignee of a card arriving on
// the board. In order: the first matching tag rule that names a principal (a
// per-card override); else the board's assignment pool, one available member
// chosen by its strategy; else the board's default principal; else, when a
// pool is set, a recorded skip. Nil when the board names nobody at all.
//
// Every principal is checked with principal-store now rather than trusted
// from the day it was set, and the check runs BEFORE the card is created, so
// a setting that has gone bad — or a principal-store that cannot answer —
// refuses the card loudly instead of leaving a noteboard item behind with no
// placement.
func (a *API) assigneeOnArrival(boardID string, cardTags []string) (*arrivalAssignment, error) {
	resolved, err := a.effectiveDefaultsFor(boardID, cardTags)
	if err != nil {
		return nil, err
	}
	chosen, named := resolved.Defaults[model.DefaultFieldPrincipal]
	if named && chosen.Source.Kind == model.DefaultSourceTagRule {
		if err := a.checkDefaultAssigneeIsAssignable(boardID, chosen); err != nil {
			return nil, err
		}
		return &arrivalAssignment{principalID: chosen.Value, detail: map[string]any{
			"principal_id": chosen.Value, "source": "tag_rule",
			"rule_id": chosen.Source.RuleID, "rule_tags": chosen.Source.RuleTags,
		}}, nil
	}
	board, err := a.store.GetBoard(boardID)
	if err != nil {
		return nil, err
	}
	pool := board.AssignmentPool
	if pool != nil {
		members, err := a.principals.ListAvailableMembers(pool.PrincipalID, time.Now())
		if errors.Is(err, principalstore.ErrNotFound) {
			return nil, &settingsCheckFailure{400, fmt.Sprintf("board %s assignment_pool.principal_id %s does not exist in principal-store; change it with PATCH /api/boards/%s", boardID, pool.PrincipalID, boardID)}
		}
		if err != nil {
			return nil, &settingsCheckFailure{502, fmt.Sprintf("principal-store could not say who in board %s's assignment pool %s is available: %s", boardID, pool.PrincipalID, err.Error())}
		}
		if len(members) > 0 {
			picked, err := a.pickFromAssignmentPool(boardID, pool.Strategy, members)
			if err != nil {
				return nil, err
			}
			return &arrivalAssignment{principalID: picked, detail: map[string]any{
				"principal_id": picked, "source": "pool", "strategy": pool.Strategy,
				"pool_principal_id": pool.PrincipalID, "candidates": len(members),
			}}, nil
		}
	}
	if named {
		if err := a.checkDefaultAssigneeIsAssignable(boardID, chosen); err != nil {
			return nil, err
		}
		detail := map[string]any{"principal_id": chosen.Value, "source": "board_default"}
		if pool != nil {
			detail["pool_empty"] = true
			detail["pool_principal_id"] = pool.PrincipalID
		}
		return &arrivalAssignment{principalID: chosen.Value, detail: detail}, nil
	}
	if pool != nil {
		return &arrivalAssignment{skipped: true, detail: map[string]any{
			"reason": "pool_empty", "pool_principal_id": pool.PrincipalID,
		}}, nil
	}
	return nil, nil
}

// checkDefaultAssigneeIsAssignable re-asks principal-store about a default
// principal, and names the setting to fix when the answer is no.
func (a *API) checkDefaultAssigneeIsAssignable(boardID string, chosen model.EffectiveDefault) error {
	err := a.checkPrincipalIsAssignable(chosen.Value)
	if err == nil {
		return nil
	}
	var failure *settingsCheckFailure
	if !errors.As(err, &failure) {
		return err
	}
	where := fmt.Sprintf("board %s default assignee", boardID)
	fix := "clear or change the board's default_principal_id"
	if chosen.Source.Kind == model.DefaultSourceTagRule {
		where = fmt.Sprintf("board %s tag rule %s (tags %s) default assignee", boardID, chosen.Source.RuleID, strings.Join(chosen.Source.RuleTags, " + "))
		fix = fmt.Sprintf("change that rule's default_principal_id with PUT /api/boards/%s/tag-rules", boardID)
	}
	return &settingsCheckFailure{failure.status, fmt.Sprintf("%s cannot be applied — %s; %s", where, failure.message, fix)}
}

// pickFromAssignmentPool chooses one of the available members by the pool's
// strategy, weighing each by its assignments on this board's cards.
// least_open_cards counts the cards in a column that does not stop the budget
// clock — the column's own declaration that work there is finished — and
// breaks a tie the way round_robin chooses. round_robin takes the member whose
// newest assignment here is the oldest, a member never assigned here first.
// A remaining tie goes to the lower principal id, so the choice never depends
// on the order principal-store lists members in.
func (a *API) pickFromAssignmentPool(boardID string, strategy model.AssignmentStrategy, members []principalstore.Principal) (string, error) {
	assignments, err := a.store.ListAssignmentsOnBoard(boardID)
	if err != nil {
		return "", err
	}
	openCards := map[string]int{}
	newestAssignment := map[string]time.Time{}
	for _, assignment := range assignments {
		if !assignment.ColumnStopsTheClock {
			openCards[assignment.PrincipalID]++
		}
		if assignment.CreatedAt.After(newestAssignment[assignment.PrincipalID]) {
			newestAssignment[assignment.PrincipalID] = assignment.CreatedAt
		}
	}
	candidates := make([]string, 0, len(members))
	for _, member := range members {
		candidates = append(candidates, member.ID)
	}
	byRoundRobin := func(left, right string) int {
		if c := newestAssignment[left].Compare(newestAssignment[right]); c != 0 {
			return c
		}
		return strings.Compare(left, right)
	}
	switch strategy {
	case model.AssignmentStrategyLeastOpenCards:
		slices.SortFunc(candidates, func(left, right string) int {
			if c := cmp.Compare(openCards[left], openCards[right]); c != 0 {
				return c
			}
			return byRoundRobin(left, right)
		})
	case model.AssignmentStrategyRoundRobin:
		slices.SortFunc(candidates, byRoundRobin)
	default:
		return "", fmt.Errorf("board %s assignment_pool.strategy %q is not one this store runs (%v)", boardID, strategy, model.AssignmentStrategies)
	}
	return candidates[0], nil
}

// applyAssigneeOnArrival carries out what assigneeOnArrival chose, unless
// someone is already on the card — an arrival fills a blank, it never
// overrides an assignment a person made. The assigned event's detail says
// where the choice came from: a tag rule, the pool, or the board default. A
// skip is recorded as an assignment_skipped event on the board. Returns the
// assignments the card carries afterwards, for the card view the caller
// answers with.
func (a *API) applyAssigneeOnArrival(cardID, boardID string, arrival *arrivalAssignment, actor string) ([]model.CardAssignment, error) {
	existing, err := a.store.ListCardAssignments(cardID)
	if err != nil {
		return nil, err
	}
	if arrival == nil || len(existing) > 0 {
		return existing, nil
	}
	detailJSON, err := json.Marshal(arrival.detail)
	if err != nil {
		return nil, err
	}
	if arrival.skipped {
		state, err := a.clockStateCarriedForward(cardID)
		if err != nil {
			return nil, err
		}
		if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
			CardID: cardID, BoardID: boardID, Kind: model.EventAssignmentSkipped, ClockState: state, Actor: actor,
			Summary: "nobody in assignment pool " + arrival.detail["pool_principal_id"].(string) + " is available",
			Detail:  json.RawMessage(detailJSON),
		}); err != nil {
			return nil, err
		}
		return existing, nil
	}
	assignment, created, err := a.store.AssignPrincipalToCard(cardID, arrival.principalID, actor)
	if err != nil {
		return nil, err
	}
	if !created {
		return existing, nil
	}
	state, err := a.clockStateCarriedForward(cardID)
	if err != nil {
		return nil, err
	}
	if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
		CardID: cardID, Kind: model.EventAssigned, ClockState: state, Actor: actor,
		Summary:    arrival.principalID,
		Detail:     json.RawMessage(detailJSON),
		OccurredAt: assignment.CreatedAt,
	}); err != nil {
		return nil, err
	}
	return []model.CardAssignment{*assignment}, nil
}

func (a *API) assignmentStrategies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, model.AssignmentStrategies)
}
