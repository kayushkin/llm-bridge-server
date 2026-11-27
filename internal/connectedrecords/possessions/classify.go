package possessions

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// ClassificationRun says what ClassifyKindless did.
type ClassificationRun struct {
	Looked     int                 `json:"looked"`
	Classified []string            `json:"classified"` // possessions given a kind
	Asked      []string            `json:"asked"`      // detail questions raised where the reader was unsure
	Left       []map[string]string `json:"left"`       // possessions left with no kind, and why
	CostUSD    float64             `json:"cost_usd"`
}

var classificationSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":{"type":"array","items":{
	"type":"object","additionalProperties":false,"required":["possession_id","kind","kind_fields","room","sure","reason"],"properties":{
	"possession_id":{"type":"string"},
	"kind":{"type":"string","enum":` + mustJSON(append(KindNames(), "none")) + `},
	"kind_fields":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["name","value"],"properties":{"name":{"type":"string"},"value":{"type":"string"}}}},
	"room":{"type":"string","enum":` + mustJSON(append(append([]string{}, Rooms...), "")) + `},
	"sure":{"type":"boolean","description":"true only when the picture and description leave no real doubt about the kind"},
	"reason":{"type":"string","description":"A few words: what it is"}}}}}}`)

// ClassifyKindless asks the first reader to give a kind to every live
// possession that has none, batchSize at a time with their pictures. A sure
// answer is written at once; an unsure one becomes a detail question on the
// possession's sighting, whose first choice gives it that kind.
func (reviewer *Reviewer) ClassifyKindless(ctx context.Context, actor string, batchSize int) (ClassificationRun, error) {
	store := reviewer.Store
	if actor == "" {
		return ClassificationRun{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	if batchSize <= 0 || batchSize > 20 {
		return ClassificationRun{}, fmt.Errorf("%w: batch_size is 1 to 20", ErrInvalid)
	}
	kindless, err := store.ListPossessions(PossessionFilter{Kind: "none", Limit: 1000})
	if err != nil {
		return ClassificationRun{}, err
	}
	run := ClassificationRun{Classified: []string{}, Asked: []string{}, Left: []map[string]string{}}
	for start := 0; start < len(kindless); start += batchSize {
		batch := kindless[start:min(start+batchSize, len(kindless))]
		var listing strings.Builder
		images := []msg.OneShotImage{}
		for _, possession := range batch {
			id := possession["id"].(string)
			picture := "no picture"
			if crop, err := reviewer.PossessionImage(ctx, id); err == nil {
				images = append(images, msg.OneShotImage{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(crop)})
				picture = fmt.Sprintf("picture %d", len(images))
			} else if !errors.Is(err, ErrNotFound) {
				return run, err
			}
			fmt.Fprintf(&listing, "- %s (%s): %s | brand %q model %q | %s | kept: %s\n", id, picture, possession["name"], possession["brand"], possession["model"],
				truncateWords(possession["description"].(string), 60), possession["spot"])
		}
		prompt := fmt.Sprintf(`These possessions in the owner's inventory have no kind yet. The pictures are in the order the list names them.

%s
For each, give its kind and the kind's fields you can tell, and the room it is in when the description or picture says. Use kind "none" only for a thing no kind fits or that cannot be told from what is here. Say sure only when there is no real doubt.`, listing.String())
		answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, readerSystemPrompt(), prompt, classificationSchema, images, 16000)
		if err != nil {
			return run, fmt.Errorf("classify possessions %d to %d: %w", start+1, start+len(batch), err)
		}
		run.CostUSD += answer.CostUSD
		var parsed struct {
			Items []struct {
				PossessionID string       `json:"possession_id"`
				Kind         string       `json:"kind"`
				KindFields   []fieldValue `json:"kind_fields"`
				Room         string       `json:"room"`
				Sure         bool         `json:"sure"`
				Reason       string       `json:"reason"`
			} `json:"items"`
		}
		if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
			return run, fmt.Errorf("%s's classification is not one: %w", answer.Model, err)
		}
		inBatch := map[string]bool{}
		for _, possession := range batch {
			inBatch[possession["id"].(string)] = true
		}
		answered := map[string]bool{}
		for _, item := range parsed.Items {
			if !inBatch[item.PossessionID] || answered[item.PossessionID] {
				continue
			}
			answered[item.PossessionID] = true
			run.Looked++
			proposal := normalizeProposal(readerItem{Kind: item.Kind, KindFields: item.KindFields, Room: item.Room})
			// Only the kind, its fields and the room: never the name or quantity.
			proposal.Name, proposal.Quantity = "", 0
			outcome, why, err := store.applyClassification(ctx, item.PossessionID, proposal, item.Sure, item.Reason, answer.Model, actor)
			if err != nil {
				return run, fmt.Errorf("apply the classification of %s: %w", item.PossessionID, err)
			}
			switch outcome {
			case "classified":
				run.Classified = append(run.Classified, item.PossessionID)
			case "asked":
				run.Asked = append(run.Asked, item.PossessionID)
			default:
				run.Left = append(run.Left, map[string]string{"possession_id": item.PossessionID, "why": why})
			}
		}
		for _, possession := range batch {
			if id := possession["id"].(string); !answered[id] {
				run.Left = append(run.Left, map[string]string{"possession_id": id, "why": answer.Model + " did not answer for it"})
			}
		}
	}
	return run, nil
}

func (s *Store) applyClassification(ctx context.Context, possessionID string, proposal Proposal, sure bool, reason, model, actor string) (string, string, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer transaction.Rollback()
	existing, err := possessionView(transaction, possessionID, false)
	if err != nil {
		return "", "", err
	}
	if existing["kind"].(string) != "" {
		return "left", "it was given a kind meanwhile", nil
	}
	own := map[string]json.RawMessage{}
	if proposal.Room != "" && existing["room"] == "" {
		own["room"], _ = json.Marshal(proposal.Room)
	}
	outcome, why := "left", ""
	switch {
	case proposal.Kind == "":
		why = fmt.Sprintf("%s found no kind: %s", model, reason)
		if len(proposal.DroppedFields) > 0 {
			why += " (dropped: " + strings.Join(proposal.DroppedFields, "; ") + ")"
		}
	case sure:
		kind, _ := KindByName(proposal.Kind)
		if err := s.patchPossessionInTransaction(ctx, transaction, possessionID, own, &kind, proposal.KindFields, actor); err != nil {
			return "", "", err
		}
		return "classified", "", transaction.Commit()
	default:
		var sightingID string
		err := transaction.QueryRow(`SELECT sighting_id FROM possession_photos WHERE possession_id = ? AND sighting_id IS NOT NULL ORDER BY is_cover DESC, linked_at LIMIT 1`, possessionID).Scan(&sightingID)
		if errors.Is(err, sql.ErrNoRows) {
			why = fmt.Sprintf("%s was unsure (%s) and the possession has no sighting to ask on", model, reason)
			break
		}
		if err != nil {
			return "", "", err
		}
		// The proposal carries no name, so an answer never renames; the
		// words shown take the possession's own.
		shown := proposal
		shown.Name = existing["name"].(string)
		question := fmt.Sprintf("%s has no kind yet. %s thinks it is %s (%s). Is it?", existing["name"], model, shown.summary(), reason)
		choices := []Choice{{Label: "Yes: " + shown.summary(), Resolution: Resolution{Action: "amend", Proposal: &proposal}}, {Label: "Leave it without a kind", Resolution: Resolution{Action: "keep"}}}
		if err := insertQuestion(transaction, sightingID, question, choices, "detail", s.unixNow()); err != nil {
			return "", "", err
		}
		outcome = "asked"
	}
	if len(own) > 0 {
		if err := s.patchPossessionInTransaction(ctx, transaction, possessionID, own, nil, nil, actor); err != nil {
			return "", "", err
		}
	}
	return outcome, why, transaction.Commit()
}
