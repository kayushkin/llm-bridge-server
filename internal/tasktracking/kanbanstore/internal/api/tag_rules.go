package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
)

// Tag rules and the effective defaults they produce.
//
//	GET  /api/boards/{id}/tag-rules                            the ordered list
//	PUT  /api/boards/{id}/tag-rules                            replace it, in order
//	GET  /api/boards/{id}/effective-defaults?tag=…&tag=…       what a card with these tags gets
//	GET  /api/boards/{id}/cards/{card_id}/effective-defaults   the same, reading the card's tags from noteboard
//
// The resolution lives in model.ResolveEffectiveDefaults and nowhere else, so
// the dispatchers and the UI ask here rather than re-implementing the order.

func (a *API) boardTagRules(w http.ResponseWriter, r *http.Request, boardID string) {
	switch r.Method {
	case "GET":
		rules, err := a.store.GetBoardTagRules(boardID)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, rules)
	case "PUT":
		if _, err := a.store.GetBoard(boardID); err != nil {
			mapDBErr(w, err)
			return
		}
		var req model.SetBoardTagRulesRequest
		decoder := json.NewDecoder(r.Body)
		// A misspelled key ("instance_id") would otherwise save a rule that
		// silently sets nothing it was meant to.
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.checkTagRuleDefaults(req.Rules); err != nil {
			writeSettingsCheckFailure(w, err)
			return
		}
		rules, err := a.store.SetBoardTagRules(boardID, req.Rules)
		if errors.Is(err, db.ErrTagRuleNotOnBoard) {
			writeError(w, 400, err.Error())
			return
		}
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, rules)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// checkTagRuleDefaults asks each owner about every id the rules name, exactly
// as a board's own defaults are checked: 400 when the owner says no, 502 when
// it cannot answer, nothing written. An id several rules share is asked once.
func (a *API) checkTagRuleDefaults(rules []model.BoardTagRuleInput) error {
	checked := map[string]bool{}
	for index, rule := range rules {
		for _, field := range model.DefaultFields {
			value := rule.DefaultValue(field)
			if value == "" {
				continue
			}
			key := string(field) + "\x00" + value
			if checked[key] {
				continue
			}
			request := &model.UpdateBoardRequest{}
			switch field {
			case model.DefaultFieldPrincipal:
				request.DefaultPrincipalID = &value
			case model.DefaultFieldAgent:
				request.DefaultAgentID = &value
			case model.DefaultFieldInstance:
				request.DefaultInstanceID = &value
			case model.DefaultFieldBundle:
				request.DefaultBundleID = &value
			}
			if err := a.checkBoardSettings(request); err != nil {
				var failure *settingsCheckFailure
				if errors.As(err, &failure) {
					return &settingsCheckFailure{failure.status, fmt.Sprintf("rules[%d] (tags %s): %s", index, strings.Join(rule.Tags, " + "), failure.message)}
				}
				return err
			}
			checked[key] = true
		}
	}
	return nil
}

func (a *API) boardEffectiveDefaults(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	resolved, err := a.effectiveDefaultsFor(boardID, r.URL.Query()["tag"])
	if err != nil {
		mapDBErr(w, err)
		return
	}
	writeJSON(w, 200, resolved)
}

func (a *API) cardEffectiveDefaults(w http.ResponseWriter, r *http.Request, boardID, cardID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	item, err := a.noteboard.GetItem(cardID)
	if noteboard.IsNotFound(err) {
		writeError(w, 404, "noteboard item not found: "+boundedtext.Value(cardID))
		return
	}
	if err != nil {
		writeError(w, 502, "noteboard read of card "+boundedtext.Value(cardID)+" failed: "+err.Error())
		return
	}
	tags, err := tagsOfNoteboardItem(item)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	resolved, err := a.effectiveDefaultsFor(boardID, tags)
	if err != nil {
		mapDBErr(w, err)
		return
	}
	resolved.CardID = cardID
	writeJSON(w, 200, resolved)
}

func (a *API) effectiveDefaultsFor(boardID string, tags []string) (*model.EffectiveDefaults, error) {
	board, err := a.store.GetBoard(boardID)
	if err != nil {
		return nil, err
	}
	rules, err := a.store.GetBoardTagRules(boardID)
	if err != nil {
		return nil, err
	}
	resolved := model.ResolveEffectiveDefaults(board, rules.Rules, tags)
	return &resolved, nil
}

// tagsOfNoteboardItem reads a card's tags off its noteboard item. Noteboard
// owns tags; an item carrying something other than a list of strings there is
// reported rather than read as untagged, because untagged would silently skip
// every rule.
func tagsOfNoteboardItem(item noteboard.Item) ([]string, error) {
	raw, present := item["tags"]
	if !present || raw == nil {
		return []string{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("noteboard item %v carries tags of type %T, not a list", item["id"], raw)
	}
	tags := make([]string, 0, len(list))
	for _, value := range list {
		tag, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("noteboard item %v carries a tag that is not a string: %v", item["id"], value)
		}
		tags = append(tags, tag)
	}
	return tags, nil
}
