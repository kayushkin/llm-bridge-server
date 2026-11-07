package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// Looks and slots. ReadLooks shows both readers all of one day's selfies at
// once: the first lists the looks (one set of clothes each) and every piece
// worn in each, with a box on each selfie that shows it; the second checks
// that list against the same photos and writes the final one, keeping,
// correcting or adding slots and naming the ones it drops. Slots are then
// identified (identify.go) and outfits rebuilt from looks (rebuild.go).

// Look is one set of clothes worn on one day, as the API answers it.
type Look struct {
	ID        string   `json:"id"`
	WornOn    string   `json:"worn_on"`
	OutfitID  *string  `json:"outfit_id"`
	PhotoIDs  []string `json:"photo_ids"`
	Slots     []Slot   `json:"slots"`
	CreatedBy string   `json:"created_by"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// Slot is one piece worn in a look.
type Slot struct {
	ID           string          `json:"id"`
	LookID       string          `json:"look_id"`
	Slot         string          `json:"slot"`
	Group        string          `json:"group"`
	Proposed     Proposal        `json:"proposed"`
	Agreement    string          `json:"agreement"`
	PossessionID *string         `json:"possession_id"`
	DecidedBy    string          `json:"decided_by"`
	Candidates   []SlotCandidate `json:"candidates"`
	Photos       []SlotPhoto     `json:"photos"`
	CreatedBy    string          `json:"created_by"`
	CreatedAt    int64           `json:"created_at"`
	UpdatedAt    int64           `json:"updated_at"`
}

// SlotCandidate is one piece a slot may be, with a likelihood: a combined
// score of both readers' clusterings, 0 to 1, not a measured probability.
type SlotCandidate struct {
	PossessionID string  `json:"possession_id"`
	Likelihood   float64 `json:"likelihood"`
}

// SlotPhoto is where a slot is on one selfie.
type SlotPhoto struct {
	PhotoID    string  `json:"photo_id"`
	Box        Box     `json:"box"`
	SightingID *string `json:"sighting_id"`
}

func lookByID(q querier, id string) (Look, error) {
	var look Look
	var outfitID sql.NullString
	err := q.QueryRow(`SELECT id, worn_on, outfit_id, created_by, created_at, updated_at FROM outfit_looks WHERE id = ?`, id).Scan(
		&look.ID, &look.WornOn, &outfitID, &look.CreatedBy, &look.CreatedAt, &look.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Look{}, fmt.Errorf("%w: no look %s", ErrNotFound, id)
	}
	if err != nil {
		return Look{}, err
	}
	if outfitID.Valid {
		look.OutfitID = &outfitID.String
	}
	if look.PhotoIDs, err = stringColumn(q, `SELECT photo_id FROM outfit_look_photos WHERE look_id = ? ORDER BY photo_id`, id); err != nil {
		return Look{}, err
	}
	slotIDs, err := stringColumn(q, `SELECT id FROM outfit_slots WHERE look_id = ? ORDER BY seq`, id)
	if err != nil {
		return Look{}, err
	}
	look.Slots = []Slot{}
	for _, slotID := range slotIDs {
		slot, err := slotByID(q, slotID)
		if err != nil {
			return Look{}, err
		}
		look.Slots = append(look.Slots, slot)
	}
	return look, nil
}

func slotByID(q querier, id string) (Slot, error) {
	var slot Slot
	var proposed, candidates string
	var possessionID sql.NullString
	err := q.QueryRow(`SELECT id, look_id, slot, group_key, proposed, agreement, possession_id, decided_by, candidates, created_by, created_at, updated_at FROM outfit_slots WHERE id = ?`, id).Scan(
		&slot.ID, &slot.LookID, &slot.Slot, &slot.Group, &proposed, &slot.Agreement, &possessionID, &slot.DecidedBy, &candidates, &slot.CreatedBy, &slot.CreatedAt, &slot.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Slot{}, fmt.Errorf("%w: no slot %s", ErrNotFound, id)
	}
	if err != nil {
		return Slot{}, err
	}
	if possessionID.Valid {
		slot.PossessionID = &possessionID.String
	}
	if err := json.Unmarshal([]byte(proposed), &slot.Proposed); err != nil {
		return Slot{}, fmt.Errorf("%s holds a proposal that is not JSON: %w", id, err)
	}
	if err := json.Unmarshal([]byte(candidates), &slot.Candidates); err != nil {
		return Slot{}, fmt.Errorf("%s holds candidates that are not JSON: %w", id, err)
	}
	rows, err := q.Query(`SELECT photo_id, box_left, box_top, box_right, box_bottom, sighting_id FROM outfit_slot_photos WHERE slot_id = ? ORDER BY photo_id`, id)
	if err != nil {
		return Slot{}, err
	}
	defer rows.Close()
	slot.Photos = []SlotPhoto{}
	for rows.Next() {
		var photo SlotPhoto
		var sightingID sql.NullString
		if err := rows.Scan(&photo.PhotoID, &photo.Box.Left, &photo.Box.Top, &photo.Box.Right, &photo.Box.Bottom, &sightingID); err != nil {
			return Slot{}, err
		}
		if sightingID.Valid {
			photo.SightingID = &sightingID.String
		}
		slot.Photos = append(slot.Photos, photo)
	}
	return slot, rows.Err()
}

// GetLook answers one look with its slots.
func (s *Store) GetLook(id string) (Look, error) { return lookByID(s.db, id) }

// GetSlot answers one slot.
func (s *Store) GetSlot(id string) (Slot, error) { return slotByID(s.db, id) }

// ListLooks answers looks, by day then age; wornOn and outfitID narrow them.
func (s *Store) ListLooks(wornOn, outfitID string, limit, offset int) ([]Look, error) {
	if limit <= 0 {
		limit = 100
	}
	ids, err := stringColumn(s.db, `SELECT id FROM outfit_looks WHERE (? = '' OR worn_on = ?) AND (? = '' OR outfit_id = ?) ORDER BY worn_on, seq LIMIT ? OFFSET ?`,
		wornOn, wornOn, outfitID, outfitID, limit, offset)
	if err != nil {
		return nil, err
	}
	looks := make([]Look, 0, len(ids))
	for _, id := range ids {
		look, err := lookByID(s.db, id)
		if err != nil {
			return nil, err
		}
		looks = append(looks, look)
	}
	return looks, nil
}

// wearableGroupKeys lists every group a slot may be in, for the readers.
func wearableGroupKeys() []string {
	keys := []string{"watch", "eyewear"}
	for _, category := range GarmentCategories {
		keys = append(keys, "garment:"+category)
	}
	for _, garmentType := range GroupedGarmentTypes {
		keys = append(keys, "garment:"+garmentType)
	}
	for _, jewelryType := range JewelryTypes {
		if jewelryType == "necklace" || !contains(NecklaceJewelryTypes, jewelryType) {
			keys = append(keys, "jewelry:"+jewelryType)
		}
	}
	for _, bagType := range BagTypes {
		keys = append(keys, "bag_or_case:"+bagType)
	}
	return keys
}

func lookSlotSchema(checking bool) map[string]any {
	properties := map[string]any{
		"slot":        map[string]any{"type": "string", "description": "Which piece and where: \"watch, left wrist\", \"ring, right ring finger\", \"jacket\", \"t-shirt under the jacket\""},
		"group":       map[string]any{"type": "string", "enum": wearableGroupKeys()},
		"name":        map[string]any{"type": "string", "description": "What the owner would call it, short: \"gray denim jacket\""},
		"description": map[string]any{"type": "string", "description": "Color, material, pattern, print, logo, hardware: what tells it from similar pieces"},
		"kind_fields": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "value"}, "properties": map[string]any{"name": stringSchema(), "value": stringSchema()}}},
		"boxes": map[string]any{"type": "array", "description": "One box on each photo of the look where the piece can be seen", "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"photo", "box"}, "properties": map[string]any{
				"photo": map[string]any{"type": "string", "description": "P1, P2…"},
				"box": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"left", "top", "right", "bottom"},
					"properties": map[string]any{"left": map[string]any{"type": "number"}, "top": map[string]any{"type": "number"}, "right": map[string]any{"type": "number"}, "bottom": map[string]any{"type": "number"}}}}}},
	}
	required := []string{"slot", "group", "name", "description", "kind_fields", "boxes"}
	if checking {
		properties["status"] = map[string]any{"type": "string", "enum": SlotStatusWords, "description": "agreed: reader A's slot is right as it is; changed: you corrected it; added: A missed it"}
		properties["from_slot"] = map[string]any{"type": "string", "description": "The S label of reader A's slot it keeps or corrects; empty when added"}
		required = append(required, "status", "from_slot")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

// SlotStatusWords are what the second reader says of each slot.
var SlotStatusWords = []string{"agreed", "changed", "added"}

func lookSchema(checking bool) json.RawMessage {
	look := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"photos", "slots"}, "properties": map[string]any{
		"photos": map[string]any{"type": "array", "items": stringSchema(), "description": "The photos (P1…) showing this set of clothes"},
		"slots":  map[string]any{"type": "array", "items": lookSlotSchema(checking)},
	}}
	properties := map[string]any{"looks": map[string]any{"type": "array", "items": look}}
	required := []string{"looks"}
	if checking {
		properties["removed"] = map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"slot", "reason"},
			"properties": map[string]any{"slot": stringSchema(), "reason": stringSchema()}}}
		required = append(required, "removed")
	}
	encoded, err := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties})
	if err != nil {
		panic(err)
	}
	return encoded
}

type readSlot struct {
	Slot        string       `json:"slot"`
	Group       string       `json:"group"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	KindFields  []fieldValue `json:"kind_fields"`
	Boxes       []struct {
		Photo string `json:"photo"`
		Box   Box    `json:"box"`
	} `json:"boxes"`
	Status   string `json:"status,omitempty"`
	FromSlot string `json:"from_slot,omitempty"`
}

type readLook struct {
	Photos []string   `json:"photos"`
	Slots  []readSlot `json:"slots"`
}

type lookReading struct {
	Looks   []readLook `json:"looks"`
	Removed []struct {
		Slot   string `json:"slot"`
		Reason string `json:"reason"`
	} `json:"removed,omitempty"`
}

// slotProposal turns a reader's slot into what would be recorded for it,
// filling the kind fields its group decides.
func slotProposal(slot readSlot) (Proposal, error) {
	if err := checkGroupKey(slot.Group); err != nil {
		return Proposal{}, err
	}
	kind, subType, _ := strings.Cut(slot.Group, ":")
	fields := map[string]string{}
	for _, field := range slot.KindFields {
		fields[strings.TrimSpace(field.Name)] = strings.TrimSpace(field.Value)
	}
	switch kind {
	case "garment":
		if contains(GroupedGarmentTypes, subType) {
			fields["garment_type"] = subType
			if !contains(GarmentCategories, fields["category"]) {
				fields["category"] = "accessory"
			}
		} else {
			fields["category"] = subType
		}
	case "jewelry":
		if subType != "necklace" || !contains(NecklaceJewelryTypes, fields["jewelry_type"]) {
			fields["jewelry_type"] = subType
		}
	case "bag_or_case":
		fields["bag_type"] = subType
	}
	values := []fieldValue{}
	for name, value := range fields {
		values = append(values, fieldValue{Name: name, Value: value})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	proposal := normalizeProposal(readerItem{Label: slot.Slot, Name: slot.Name, Description: slot.Description, Kind: kind, KindFields: values, Quantity: 1})
	proposal.Status = "unconfirmed"
	if proposalGroup(proposal) != slot.Group {
		return Proposal{}, fmt.Errorf("the slot %q in group %s reads as group %q (%s)", slot.Slot, slot.Group, proposalGroup(proposal), strings.Join(proposal.DroppedFields, "; "))
	}
	return proposal, nil
}

// lookSelfie is one selfie of a day being read.
type lookSelfie struct{ reviewID, photoID, wearerID string }

// ReadLooks reads every day's selfies not yet in a look, a day at a time.
func (reviewer *Reviewer) ReadLooks(ctx context.Context, actor string) (AuditRun, error) {
	store := reviewer.Store
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	rows, err := store.db.Query(`SELECT worn_on, id, photo_id, wearer_person_id FROM photo_reviews
		WHERE reading = 'outfit' AND worn_on != '' AND status IN ('done', 'waiting_for_owner') AND photo_id NOT IN (SELECT photo_id FROM outfit_look_photos)
		ORDER BY worn_on, seq`)
	if err != nil {
		return run, err
	}
	type selfie = lookSelfie
	days := map[string][]selfie{}
	order := []string{}
	for rows.Next() {
		var day string
		var item selfie
		if err := rows.Scan(&day, &item.reviewID, &item.photoID, &item.wearerID); err != nil {
			rows.Close()
			return run, err
		}
		if _, seen := days[day]; !seen {
			order = append(order, day)
		}
		days[day] = append(days[day], item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return run, err
	}
	for _, day := range order {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		selfies := days[day]
		run.Looked++
		images := []msg.OneShotImage{}
		var listing strings.Builder
		labels := map[string]selfie{}
		failed := false
		for index, item := range selfies {
			picture, _, err := reviewer.Photos.ReviewImage(ctx, item.photoID)
			if err != nil {
				run.fail(item.photoID, err)
				failed = true
				break
			}
			face, err := reviewer.Photos.WearerFace(ctx, item.photoID, item.wearerID)
			if err != nil {
				run.fail(item.photoID, err)
				failed = true
				break
			}
			label := fmt.Sprintf("P%d", index+1)
			labels[label] = item
			images = append(images, jpegImage(picture))
			fmt.Fprintf(&listing, "%s (picture %d): the owner's face is in the box left %.2f, top %.2f, right %.2f, bottom %.2f\n", label, index+1, face.Left, face.Top, face.Right, face.Bottom)
		}
		if failed {
			continue
		}
		common := fmt.Sprintf(`These are the photos the owner took of themself on %s. Boxes are fractions of each photo from the top left. Other people may be in them: describe only what the owner wears.
%s
Groups: shirts and other tops are garment:top, pants and shorts garment:bottom, jackets and coats garment:outerwear, scarves garment:scarf; necklaces, pendants and chains are jewelry:necklace. Leave out socks, and anything hidden in every photo. A pair (shoes, earrings) is one slot.`, day, listing.String())
		listPrompt := common + `

List the looks: one look when every photo shows the same clothes, more when the owner changed clothes between photos; every photo in exactly one look. For each look, list every piece the owner wears as one slot: what and where ("watch, left wrist", "ring, right ring finger", "jacket"), its group, a name, a description that tells it from similar pieces, kind fields you can see, and a box on every photo of the look where it can be seen.`
		listing1, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, auditSystemPrompt, listPrompt, lookSchema(false), images, 16000)
		if err != nil {
			run.fail(day, fmt.Errorf("reader %s: %w", reviewer.FirstReaderRole, err))
			continue
		}
		run.CostUSD += listing1.CostUSD
		var first lookReading
		if err := json.Unmarshal(listing1.Parsed, &first); err != nil {
			run.fail(day, fmt.Errorf("%s's looks are not a reading: %w", listing1.Model, err))
			continue
		}
		labeled := []map[string]any{}
		number := 0
		for _, look := range first.Looks {
			slots := []map[string]any{}
			for _, slot := range look.Slots {
				number++
				encoded, _ := json.Marshal(slot)
				var asMap map[string]any
				_ = json.Unmarshal(encoded, &asMap)
				asMap["label"] = fmt.Sprintf("S%d", number)
				slots = append(slots, asMap)
			}
			labeled = append(labeled, map[string]any{"photos": look.Photos, "slots": slots})
		}
		firstJSON, _ := json.MarshalIndent(labeled, "", "  ")
		checkPrompt := common + fmt.Sprintf(`

Reader A, made by another company, listed the looks and the pieces in them:
%s

Check this against the photos and write the final list of looks. For each slot that is right, keep it with status agreed and its S label in from_slot. Correct one that is wrong (status changed, from_slot its label): its group, name, description or boxes. Add a piece A missed (status added). Put in removed each S label that is not a real piece the owner wears, with why. Move photos between looks if A split or joined the looks wrongly; every photo in exactly one look.`, firstJSON)
		checked, err := reviewer.Models.Ask(ctx, reviewer.SecondReaderRole, auditSystemPrompt, checkPrompt, lookSchema(true), images, 16000)
		if err != nil {
			run.fail(day, fmt.Errorf("reader %s: %w", reviewer.SecondReaderRole, err))
			continue
		}
		run.CostUSD += checked.CostUSD
		var final lookReading
		if err := json.Unmarshal(checked.Parsed, &final); err != nil {
			run.fail(day, fmt.Errorf("%s's looks are not a reading: %w", checked.Model, err))
			continue
		}
		for _, removed := range final.Removed {
			run.leave(day+" "+removed.Slot, checked.Model+" removed it: "+removed.Reason)
		}
		lookIDs, slotCount, err := store.recordLooks(ctx, day, labels, final, [2]string{listing1.Model, checked.Model}, actor, &run)
		if err != nil {
			run.fail(day, err)
			continue
		}
		run.Counts["looks"] += len(lookIDs)
		run.Counts["slots"] += slotCount
		run.Actions = append(run.Actions, AuditAction{Action: "read", Records: lookIDs, Reason: fmt.Sprintf("%s: %d looks, %d slots", day, len(lookIDs), slotCount)})
	}
	return run, nil
}

// recordLooks writes one day's looks and slots in one transaction. A reading
// that leaves a photo out of every look, or puts one in two, is refused.
func (s *Store) recordLooks(ctx context.Context, day string, labels map[string]lookSelfie, reading lookReading, models [2]string, actor string, run *AuditRun) ([]string, int, error) {
	placed := map[string]bool{}
	for _, look := range reading.Looks {
		for _, label := range look.Photos {
			label = strings.TrimSpace(label)
			if _, known := labels[label]; !known {
				return nil, 0, fmt.Errorf("%s put %q in a look, which is not one of the photos", models[1], label)
			}
			if placed[label] {
				return nil, 0, fmt.Errorf("%s put %s in two looks", models[1], label)
			}
			placed[label] = true
		}
	}
	for label := range labels {
		if !placed[label] {
			return nil, 0, fmt.Errorf("%s left %s out of every look", models[1], label)
		}
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	lookIDs := []string{}
	slotCount := 0
	for _, look := range reading.Looks {
		lookID, sequence, err := nextID(transaction, "outfit_looks", "outfit_look")
		if err != nil {
			return nil, 0, err
		}
		if _, err := transaction.Exec(`INSERT INTO outfit_looks (id, seq, worn_on, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, lookID, sequence, day, actor, now, now); err != nil {
			return nil, 0, err
		}
		inLook := map[string]bool{}
		for _, label := range look.Photos {
			selfie := labels[strings.TrimSpace(label)]
			inLook[strings.TrimSpace(label)] = true
			if _, err := transaction.Exec(`INSERT INTO outfit_look_photos (look_id, photo_id, photo_review_id) VALUES (?, ?, ?)`, lookID, selfie.photoID, selfie.reviewID); err != nil {
				return nil, 0, err
			}
		}
		for _, slot := range look.Slots {
			proposal, err := slotProposal(slot)
			if err != nil {
				run.leave(day+" "+slot.Slot, err.Error())
				continue
			}
			boxes := map[string]Box{}
			for _, placedBox := range slot.Boxes {
				label := strings.TrimSpace(placedBox.Photo)
				box := placedBox.Box.clamped()
				if inLook[label] && box.valid() {
					boxes[labels[label].photoID] = box
				}
			}
			if len(boxes) == 0 {
				run.leave(day+" "+slot.Slot, "no box on a photo of its look")
				continue
			}
			agreement := map[string]string{"agreed": "both_agree", "changed": "second_reader_changed", "added": "second_reader_added"}[slot.Status]
			if agreement == "" {
				return nil, 0, fmt.Errorf("%s gave the slot %q the status %q", models[1], slot.Slot, slot.Status)
			}
			slotID, slotSequence, err := nextID(transaction, "outfit_slots", "outfit_slot")
			if err != nil {
				return nil, 0, err
			}
			encoded, _ := json.Marshal(proposal)
			if _, err := transaction.Exec(`INSERT INTO outfit_slots (id, seq, look_id, slot, group_key, proposed, agreement, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				slotID, slotSequence, lookID, strings.TrimSpace(slot.Slot), slot.Group, string(encoded), agreement, actor, now, now); err != nil {
				return nil, 0, err
			}
			for photoID, box := range boxes {
				if _, err := transaction.Exec(`INSERT INTO outfit_slot_photos (slot_id, photo_id, box_left, box_top, box_right, box_bottom) VALUES (?, ?, ?, ?, ?, ?)`,
					slotID, photoID, box.Left, box.Top, box.Right, box.Bottom); err != nil {
					return nil, 0, err
				}
			}
			slotCount++
		}
		lookIDs = append(lookIDs, lookID)
	}
	return lookIDs, slotCount, transaction.Commit()
}
