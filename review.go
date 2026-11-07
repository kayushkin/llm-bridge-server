package possessionstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The photo review, in three steps:
//
//  1. Each reader, on its own, lists the possessions it sees (round 1).
//  2. Each reader sees both lists and what the inventory already holds, and
//     writes its final list: one entry per thing, saying which round-1
//     entries it merges, whether it is something already recorded, and any
//     doubt it would ask the owner about (round 2).
//  3. The store matches the two final lists. Where they agree and neither
//     has a doubt, it records the thing. Everything else becomes a question
//     for the owner, whose answer is applied the same way.

// Reviewer runs photo reviews.
type Reviewer struct {
	Store            *Store
	Models           ModelCaller
	Photos           PhotoSource
	FirstReaderRole  string
	SecondReaderRole string
	// OwnerLocation is the owner's time zone: an outfit read from a photo
	// was worn on the photo's day there.
	OwnerLocation *time.Location
	// wake is signalled when a review is queued, so the worker need not wait
	// for its next poll.
	wakeOnce sync.Once
	wake     chan struct{}
}

func (reviewer *Reviewer) wakeChannel() chan struct{} {
	reviewer.wakeOnce.Do(func() { reviewer.wake = make(chan struct{}, 1) })
	return reviewer.wake
}

// Wake tells the worker a review is waiting.
func (reviewer *Reviewer) Wake() {
	select {
	case reviewer.wakeChannel() <- struct{}{}:
	default:
	}
}

// fieldValue is one kind field as a reader gives it: always text, so one
// schema serves every kind. A list is comma-separated.
type fieldValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type readerItem struct {
	Label       string       `json:"label"`
	Box         Box          `json:"box"`
	Name        string       `json:"name"`
	Brand       string       `json:"brand"`
	Model       string       `json:"model"`
	Description string       `json:"description"`
	Quantity    int          `json:"quantity"`
	Kind        string       `json:"kind"`
	KindFields  []fieldValue `json:"kind_fields"`
	VisibleText string       `json:"visible_text"`
	Room        string       `json:"room"`
	Confidence  float64      `json:"confidence"`
}

type firstReading struct {
	Items []readerItem `json:"items"`
}

type finalItem struct {
	readerItem
	FromReadings []string `json:"from_readings"`
	ExistingID   string   `json:"existing_id"`
	Doubt        string   `json:"doubt"`
	DoubtKind    string   `json:"doubt_kind"` // identity or detail; see itemSchema
}

type finalReading struct {
	Items []finalItem `json:"items"`
}

// Proposal is what the store would record for a sighting.
type Proposal struct {
	Name                 string                     `json:"name"`
	Brand                string                     `json:"brand"`
	Model                string                     `json:"model"`
	Description          string                     `json:"description"`
	Quantity             int                        `json:"quantity"`
	Room                 string                     `json:"room,omitempty"`
	Status               string                     `json:"status,omitempty"` // set when the record should not start as owned
	Kind                 string                     `json:"kind"`
	KindFields           map[string]json.RawMessage `json:"kind_fields"`
	DroppedFields        []string                   `json:"dropped_fields,omitempty"`
	ExistingPossessionID string                     `json:"existing_possession_id,omitempty"`
	ExistingSightingID   string                     `json:"existing_sighting_id,omitempty"`
}

// Resolution is what answering a question does to its sighting, or, for
// the wearable audit's actions (auditActions), to the records it names.
type Resolution struct {
	Action       string    `json:"action"` // ResolutionActions
	Proposal     *Proposal `json:"proposal,omitempty"`
	PossessionID string    `json:"possession_id,omitempty"`
	SightingID   string    `json:"sighting_id,omitempty"`
	// UnlinkPossessionID, with accept, same_as or reject: the sighting is
	// recorded as this possession and that link is wrong; it is undone
	// first (photo, sighting, and the piece's place in the reading's
	// outfit), then the action applies.
	UnlinkPossessionID string `json:"unlink_possession_id,omitempty"`
	// merge_possessions: PossessionID is folded into IntoPossessionID.
	// keep_apart names the same two and changes nothing.
	IntoPossessionID string `json:"into_possession_id,omitempty"`
	// combine_outfits: OutfitID is folded into IntoOutfitID after each of
	// PieceMerges. keep_outfits_apart names the two and changes nothing.
	OutfitID     string       `json:"outfit_id,omitempty"`
	IntoOutfitID string       `json:"into_outfit_id,omitempty"`
	PieceMerges  []PieceMerge `json:"piece_merges,omitempty"`
	// slot_link (with PossessionID), slot_new and slot_not_mine: the
	// outfit slot the answer relinks.
	SlotID      string `json:"slot_id,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

// PieceMerge is one possession to fold into another.
type PieceMerge struct {
	DuplicatePossessionID string `json:"duplicate_possession_id"`
	KeptPossessionID      string `json:"kept_possession_id"`
}

// Choice is one suggested answer to a question.
type Choice struct {
	Label      string     `json:"label"`
	Resolution Resolution `json:"resolution"`
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }

func itemSchema(final bool) map[string]any {
	properties := map[string]any{
		"label": map[string]any{"type": "string", "description": "A few words a person can find it by: \"black hoodie on the chair back\""},
		"box": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"left", "top", "right", "bottom"},
			"description": "Where it is, in fractions of the photo's width and height from the top left, 0 to 1",
			"properties":  map[string]any{"left": map[string]any{"type": "number"}, "top": map[string]any{"type": "number"}, "right": map[string]any{"type": "number"}, "bottom": map[string]any{"type": "number"}}},
		"name":         map[string]any{"type": "string", "description": "What the owner would call it, short"},
		"brand":        map[string]any{"type": "string", "description": "Only when you can read it or truly recognize it; otherwise empty"},
		"model":        map[string]any{"type": "string", "description": "Only when you can read it or truly recognize it; otherwise empty"},
		"description":  map[string]any{"type": "string", "description": "Color, material, condition, anything that tells it apart"},
		"quantity":     map[string]any{"type": "integer", "description": "How many identical ones, usually 1"},
		"kind":         map[string]any{"type": "string", "enum": append(KindNames(), "none")},
		"kind_fields":  map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "value"}, "properties": map[string]any{"name": stringSchema(), "value": stringSchema()}}},
		"visible_text": map[string]any{"type": "string", "description": "Any text you can read on it, as written"},
		"room":         map[string]any{"type": "string", "enum": append(append([]string{}, Rooms...), ""), "description": "The room it is in, when the photo shows; otherwise empty"},
		"confidence":   map[string]any{"type": "number", "description": "0 to 1: how sure you are it is an owned possession and identified rightly"},
	}
	required := []string{"label", "box", "name", "brand", "model", "description", "quantity", "kind", "kind_fields", "visible_text", "room", "confidence"}
	if final {
		properties["from_readings"] = map[string]any{"type": "array", "items": stringSchema(), "description": "The round-one labels (A1, B3…) this entry stands for; empty for a thing neither listed"}
		properties["existing_id"] = map[string]any{"type": "string", "description": "possession_… or sighting_… when it is something already listed below; otherwise empty"}
		properties["doubt"] = map[string]any{"type": "string", "description": "The question you would ask the owner, when something that matters is unsure; otherwise empty"}
		properties["doubt_kind"] = map[string]any{"type": "string", "enum": []string{"", "identity", "detail"},
			"description": "identity: the doubt is which thing this is (whether it is one already listed, which of several, whether it is the owner's at all, what kind of thing it is). detail: the thing is clear and the doubt is only a fact about it (material, model, size). Empty with no doubt"}
		required = append(required, "from_readings", "existing_id", "doubt", "doubt_kind")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func readingSchema(final bool) json.RawMessage {
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"items"},
		"properties": map[string]any{"items": map[string]any{"type": "array", "items": itemSchema(final)}}}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return encoded
}

// kindGuide describes every kind's fields for a prompt, from KindTypes, so
// the prompt never keeps its own copy of a vocabulary.
func kindGuide() string {
	var guide strings.Builder
	for _, kind := range KindTypes {
		fmt.Fprintf(&guide, "- %s:\n", kind.Name)
		for _, column := range kind.Columns {
			if column.References != "" {
				continue
			}
			line := fmt.Sprintf("    %s (%s", column.Name, column.Type)
			if column.Required {
				line += ", required"
			}
			if len(column.Vocabulary) > 0 {
				line += "; one of: " + strings.Join(column.Vocabulary, ", ")
			}
			if column.Minimum != nil && column.Maximum != nil {
				line += fmt.Sprintf("; %d to %d", *column.Minimum, *column.Maximum)
			}
			guide.WriteString(line + ")\n")
		}
	}
	return guide.String()
}

func readerSystemPrompt() string {
	return `You help a person build an inventory of the things they own, from photos of their home. Two readers, made by different companies, read each photo; you are one of them. Be exact: the inventory is only worth having if it is right, and a question to the owner is better than a wrong record.

What to list: anything the person owns and would want on record: clothes, shoes, bags, electronics, computers and their parts, monitors, audio gear, instruments, furniture, appliances, tools, sports gear, decor, art. Several identical things are one entry with a quantity. A shelf of books or a pile of cables is one entry.

What to leave out: the building itself (walls, doors, built-in cabinets, outlets, light fixtures), food, things used up (toiletries in use, paper, packaging), trash, and things too blurred or hidden to tell what they are.

Brand and model: give them only when you can read them or are truly sure from the design; never guess. Put the text you read in visible_text.

Kinds, with the fields each takes (give a field only when you can see it; a list field is comma-separated):
` + kindGuide() + `Anything that fits none of these is kind "none" with no kind fields.`
}

func labeledItems(prefix string, items []readerItem) string {
	labeled := make([]map[string]any, len(items))
	for index, item := range items {
		encoded, _ := json.Marshal(item)
		var asMap map[string]any
		_ = json.Unmarshal(encoded, &asMap)
		asMap["label_id"] = fmt.Sprintf("%s%d", prefix, index+1)
		labeled[index] = asMap
	}
	encoded, _ := json.MarshalIndent(labeled, "", "  ")
	return string(encoded)
}

func secondRoundPrompt(readerLetter string, readingA, readingB []readerItem, candidates string) string {
	return fmt.Sprintf(`This is round two. You are reader %s. Here is what each reader listed on its own in round one.

Reader A's list:
%s

Reader B's list:
%s

What the inventory already holds, and things seen in earlier photos that still wait on the owner's answer:
%s

Write the final list: one entry per real thing in the photo.
- Where both lists name the same thing, write one entry and put both label_ids in from_readings.
- Keep a thing only the other reader listed if, looking again, you agree it is there. Leave out a thing only if you are sure it is a mistake.
- Correct your own entries where the other reader's are better.
- Where the thing is one already listed above, put its id in existing_id. Claim a match only when the photo supports it (same color, shape, brand, marks); a black hoodie is not every black hoodie.
- When something that matters is unsure (whether two entries are one thing, a brand, whether it matches an id above), write in doubt the one question you would ask the owner. Otherwise leave doubt empty.`,
		readerLetter, labeledItems("A", readingA), labeledItems("B", readingB), candidates)
}

// candidateList describes, for round two, what the inventory holds and what
// earlier photos are still waiting on.
func (s *Store) candidateList(excludeReviewID string) (string, error) {
	var list strings.Builder
	rows, err := s.db.Query(`SELECT id, name, brand, model, spot, description FROM possessions WHERE deleted_at = 0 AND status IN ('owned', 'lent_out') ORDER BY seq DESC LIMIT 600`)
	if err != nil {
		return "", err
	}
	type candidate struct{ id, name, brand, model, spot, description string }
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.name, &c.brand, &c.model, &c.spot, &c.description); err != nil {
			rows.Close()
			return "", err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, c := range candidates {
		view, err := possessionView(s.db, c.id, false)
		if err != nil {
			return "", err
		}
		kindSummary := "no kind"
		if kindName := view["kind"].(string); kindName != "" {
			kindSummary = kindName + " " + summarizeKindRecord(view[kindName].(map[string]any))
		}
		fmt.Fprintf(&list, "- %s: %s | %s | brand %q model %q | %s | kept: %s\n", c.id, c.name, kindSummary, c.brand, c.model, truncateWords(c.description, 30), c.spot)
	}
	sightingRows, err := s.db.Query(`SELECT id, label, proposed FROM sightings WHERE status = 'proposed' AND photo_review_id != ? ORDER BY seq`, excludeReviewID)
	if err != nil {
		return "", err
	}
	defer sightingRows.Close()
	for sightingRows.Next() {
		var id, label, proposed string
		if err := sightingRows.Scan(&id, &label, &proposed); err != nil {
			return "", err
		}
		var proposal Proposal
		if err := json.Unmarshal([]byte(proposed), &proposal); err != nil {
			return "", fmt.Errorf("%s holds a proposal that is not JSON: %w", id, err)
		}
		fmt.Fprintf(&list, "- %s (seen in an earlier photo, waiting on the owner): %s | %s | brand %q | %s\n", id, label, proposal.Kind, proposal.Brand, truncateWords(proposal.Description, 30))
	}
	if err := sightingRows.Err(); err != nil {
		return "", err
	}
	if list.Len() == 0 {
		return "(nothing yet)", nil
	}
	return list.String(), nil
}

func summarizeKindRecord(record map[string]any) string {
	parts := []string{}
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "id" {
			continue
		}
		switch value := record[key].(type) {
		case string:
			if value != "" {
				parts = append(parts, key+"="+value)
			}
		case int64:
			parts = append(parts, key+"="+strconv.FormatInt(value, 10))
		case []string:
			if len(value) > 0 {
				parts = append(parts, key+"="+strings.Join(value, ","))
			}
		}
	}
	return strings.Join(parts, " ")
}

func truncateWords(text string, limit int) string {
	words := strings.Fields(text)
	if len(words) <= limit {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:limit], " ") + "…"
}

// normalizeProposal turns a reader's entry into a Proposal, holding every
// kind field to its column. A field that does not fit is dropped and named in
// DroppedFields, so the owner can see what a reader said; a kind whose
// required field is missing is dropped the same way.
func normalizeProposal(item readerItem) Proposal {
	proposal := Proposal{Name: strings.TrimSpace(item.Name), Brand: strings.TrimSpace(item.Brand), Model: strings.TrimSpace(item.Model),
		Description: strings.TrimSpace(item.Description), Quantity: max(item.Quantity, 1), KindFields: map[string]json.RawMessage{}}
	if room := strings.TrimSpace(item.Room); room != "" {
		if contains(Rooms, room) {
			proposal.Room = room
		} else {
			proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("room: %s (not one of %s)", room, strings.Join(Rooms, ", ")))
		}
	}
	if proposal.Name == "" {
		proposal.Name = strings.TrimSpace(item.Label)
	}
	if text := strings.TrimSpace(item.VisibleText); text != "" && !strings.Contains(proposal.Description, text) {
		proposal.Description = strings.TrimSpace(proposal.Description + "\n\nText on it: " + text)
	}
	kind, known := KindByName(item.Kind)
	if !known {
		for _, field := range item.KindFields {
			proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("%s: %s (kind %q has no such field)", field.Name, field.Value, item.Kind))
		}
		return proposal
	}
	for _, field := range item.KindFields {
		column, found := kind.column(field.Name)
		value := strings.TrimSpace(field.Value)
		if value == "" {
			continue
		}
		if !found || column.References != "" {
			proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("%s: %s (not a field of %s)", field.Name, value, kind.Name))
			continue
		}
		var raw json.RawMessage
		switch column.Type {
		case ColumnInteger:
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("%s: %s (not a whole number)", field.Name, value))
				continue
			}
			raw = json.RawMessage(value)
		case ColumnStringList:
			items := []string{}
			for _, part := range strings.Split(value, ",") {
				if part = strings.TrimSpace(part); part != "" {
					items = append(items, part)
				}
			}
			raw, _ = json.Marshal(items)
		default:
			raw, _ = json.Marshal(value)
		}
		if _, err := column.decode(raw); err != nil {
			proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("%s: %s (%v)", field.Name, value, err))
			continue
		}
		proposal.KindFields[field.Name] = raw
	}
	for _, column := range kind.Columns {
		if column.Required {
			if _, present := proposal.KindFields[column.Name]; !present {
				proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("kind %s (its %s was not given)", kind.Name, column.Name))
				for name, raw := range proposal.KindFields {
					proposal.DroppedFields = append(proposal.DroppedFields, fmt.Sprintf("%s: %s", name, raw))
				}
				proposal.KindFields = map[string]json.RawMessage{}
				return proposal
			}
		}
	}
	proposal.Kind = kind.Name
	return proposal
}

// possessionFields turns a proposal into the fields CreatePossession takes.
func (proposal Proposal) possessionFields() map[string]json.RawMessage {
	text := func(value string) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }
	fields := map[string]json.RawMessage{
		"name": text(proposal.Name), "brand": text(proposal.Brand), "model": text(proposal.Model),
		"description": text(proposal.Description), "quantity": json.RawMessage(strconv.Itoa(max(proposal.Quantity, 1))),
	}
	if proposal.Room != "" {
		fields["room"] = text(proposal.Room)
	}
	if proposal.Status != "" {
		fields["status"] = text(proposal.Status)
	}
	if len(proposal.DroppedFields) > 0 {
		fields["description"] = text(strings.TrimSpace(proposal.Description + "\n\nThe photo readers also said, in words the inventory could not take: " + strings.Join(proposal.DroppedFields, "; ")))
	}
	if proposal.Kind != "" {
		kindFields, _ := json.Marshal(proposal.KindFields)
		fields[proposal.Kind] = kindFields
	}
	return fields
}

func (proposal Proposal) summary() string {
	parts := []string{proposal.Name}
	if proposal.Brand != "" || proposal.Model != "" {
		parts = append(parts, strings.TrimSpace(proposal.Brand+" "+proposal.Model))
	}
	if proposal.Kind != "" {
		kindParts := []string{proposal.Kind}
		names := make([]string, 0, len(proposal.KindFields))
		for name := range proposal.KindFields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			kindParts = append(kindParts, name+" "+strings.Trim(string(proposal.KindFields[name]), `"`))
		}
		parts = append(parts, strings.Join(kindParts, ", "))
	}
	if proposal.Quantity > 1 {
		parts = append(parts, fmt.Sprintf("%d of them", proposal.Quantity))
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// The queue.
// ---------------------------------------------------------------------------

// PhotoReview is a photo going through review, with what came of it.
type PhotoReview struct {
	ID          string         `json:"id"`
	PhotoID     string         `json:"photo_id"`
	Status      string         `json:"status"`
	ImageSource string         `json:"image_source"`
	Error       string         `json:"error"`
	QueuedBy    string         `json:"queued_by"`
	QueuedAt    int64          `json:"queued_at"`
	FinishedAt  int64          `json:"finished_at"`
	Reading     string         `json:"reading"`          // inventory or outfit (PhotoReviewReadings)
	WearerID    string         `json:"wearer_person_id"` // an outfit reading: whose outfit, a people-store person_…
	OutfitID    *string        `json:"outfit_id"`        // an outfit reading: the outfit it recorded
	WornOn      string         `json:"worn_on"`          // an outfit reading: the photo's day in the owner's time zone
	Readings    []ModelReading `json:"readings,omitempty"`
	Sightings   []Sighting     `json:"sightings,omitempty"`
	CostUSD     float64        `json:"cost_usd"`
}

// ModelReading is what one reader said in one round.
type ModelReading struct {
	Round        int             `json:"round"`
	ModelRole    string          `json:"model_role"`
	ModelID      string          `json:"model_id"`
	Answer       json.RawMessage `json:"answer"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	CostUSD      float64         `json:"cost_usd"`
	ReadAt       int64           `json:"read_at"`
}

// Sighting is one thing the review found in a photo.
type Sighting struct {
	ID               string              `json:"id"`
	PhotoReviewID    string              `json:"photo_review_id"`
	PhotoID          string              `json:"photo_id"`
	Label            string              `json:"label"`
	Box              Box                 `json:"box"`
	Proposed         Proposal            `json:"proposed"`
	Versions         map[string]Proposal `json:"versions"`
	Agreement        string              `json:"agreement"`
	Status           string              `json:"status"`
	SameAsSightingID *string             `json:"same_as_sighting_id"`
	PossessionID     *string             `json:"possession_id"`
	DecidedBy        string              `json:"decided_by"`
	DecidedAt        int64               `json:"decided_at"`
	QuestionIDs      []string            `json:"question_ids"`
}

// OwnerQuestion is a question for the owner about a sighting.
type OwnerQuestion struct {
	ID           string          `json:"id"`
	SightingID   string          `json:"sighting_id"`
	PhotoID      string          `json:"photo_id"`
	Purpose      string          `json:"purpose"`  // decision: the sighting waits on it; detail: it adds to what is recorded
	AskedBy      string          `json:"asked_by"` // '' when the photo's reading asked it, otherwise one of QuestionAskers
	Question     string          `json:"question"`
	Choices      []Choice        `json:"choices"`
	AnswerChoice *int            `json:"answer_choice"`
	AnswerText   string          `json:"answer_text"`
	Resolution   json.RawMessage `json:"resolution"`
	AnsweredBy   string          `json:"answered_by"`
	AskedAt      int64           `json:"asked_at"`
	AnsweredAt   int64           `json:"answered_at"`
}

// QueuePhotoReview queues a photo for a reading (PhotoReviewReadings); an
// outfit reading names whose outfit it is. A photo already queued answers its
// existing review and created false.
func (s *Store) QueuePhotoReview(ctx context.Context, photoID, queuedBy, reading, wearerPersonID string) (PhotoReview, bool, error) {
	if photoID == "" || queuedBy == "" {
		return PhotoReview{}, false, fmt.Errorf("%w: photo_id and queued_by are required", ErrInvalid)
	}
	if !contains(PhotoReviewReadings, reading) {
		return PhotoReview{}, false, fmt.Errorf("%w: reading must be one of %s", ErrInvalid, strings.Join(PhotoReviewReadings, ", "))
	}
	if (reading == "outfit") != (wearerPersonID != "") {
		return PhotoReview{}, false, fmt.Errorf("%w: an outfit reading needs wearer_person_id, and only an outfit reading takes one", ErrInvalid)
	}
	if existing, err := s.photoReviewForPhoto(photoID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return PhotoReview{}, false, err
	}
	if err := s.references.Check(ctx, ReferencePhoto, photoID); err != nil {
		return PhotoReview{}, false, err
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PhotoReview{}, false, err
	}
	defer transaction.Rollback()
	id, sequence, err := nextID(transaction, "photo_reviews", "photo_review")
	if err != nil {
		return PhotoReview{}, false, err
	}
	if _, err := transaction.Exec(`INSERT INTO photo_reviews (id, seq, photo_id, status, queued_by, queued_at, reading, wearer_person_id) VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)`, id, sequence, photoID, queuedBy, s.unixNow(), reading, wearerPersonID); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return PhotoReview{}, false, fmt.Errorf("%w: %s is already queued", ErrConflict, photoID)
		}
		return PhotoReview{}, false, err
	}
	if err := transaction.Commit(); err != nil {
		return PhotoReview{}, false, err
	}
	review, err := s.GetPhotoReview(id)
	return review, true, err
}

func (s *Store) photoReviewForPhoto(photoID string) (PhotoReview, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM photo_reviews WHERE photo_id = ?`, photoID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoReview{}, fmt.Errorf("%w: %s is not queued", ErrNotFound, photoID)
	}
	if err != nil {
		return PhotoReview{}, err
	}
	return s.GetPhotoReview(id)
}

// GetPhotoReview answers a review with its readings and sightings.
func (s *Store) GetPhotoReview(id string) (PhotoReview, error) {
	review, err := photoReviewRow(s.db, id)
	if err != nil {
		return PhotoReview{}, err
	}
	rows, err := s.db.Query(`SELECT round, model_role, model_id, answer, input_tokens, output_tokens, cost_usd, read_at FROM model_readings WHERE photo_review_id = ? ORDER BY round, model_role`, id)
	if err != nil {
		return PhotoReview{}, err
	}
	defer rows.Close()
	review.Readings = []ModelReading{}
	for rows.Next() {
		var reading ModelReading
		var answer string
		if err := rows.Scan(&reading.Round, &reading.ModelRole, &reading.ModelID, &answer, &reading.InputTokens, &reading.OutputTokens, &reading.CostUSD, &reading.ReadAt); err != nil {
			return PhotoReview{}, err
		}
		reading.Answer = json.RawMessage(answer)
		review.CostUSD += reading.CostUSD
		review.Readings = append(review.Readings, reading)
	}
	if err := rows.Err(); err != nil {
		return PhotoReview{}, err
	}
	sightingIDs, err := stringColumn(s.db, `SELECT id FROM sightings WHERE photo_review_id = ? ORDER BY seq`, id)
	if err != nil {
		return PhotoReview{}, err
	}
	review.Sightings = []Sighting{}
	for _, sightingID := range sightingIDs {
		sighting, err := sightingByID(s.db, sightingID)
		if err != nil {
			return PhotoReview{}, err
		}
		review.Sightings = append(review.Sightings, sighting)
	}
	return review, nil
}

func photoReviewRow(q querier, id string) (PhotoReview, error) {
	var review PhotoReview
	var outfitID sql.NullString
	err := q.QueryRow(`SELECT id, photo_id, status, image_source, error, queued_by, queued_at, finished_at, reading, wearer_person_id, outfit_id, worn_on FROM photo_reviews WHERE id = ?`, id).Scan(
		&review.ID, &review.PhotoID, &review.Status, &review.ImageSource, &review.Error, &review.QueuedBy, &review.QueuedAt, &review.FinishedAt,
		&review.Reading, &review.WearerID, &outfitID, &review.WornOn)
	if outfitID.Valid {
		review.OutfitID = &outfitID.String
	}
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoReview{}, fmt.Errorf("%w: no photo review %s", ErrNotFound, id)
	}
	return review, err
}

// ListPhotoReviews answers reviews, newest first, without their readings.
func (s *Store) ListPhotoReviews(status string, limit, offset int) ([]PhotoReview, error) {
	conditions := "1 = 1"
	arguments := []any{}
	if status != "" {
		if !contains(PhotoReviewStatuses, status) {
			return nil, fmt.Errorf("%w: status must be one of %s", ErrInvalid, strings.Join(PhotoReviewStatuses, ", "))
		}
		conditions = "status = ?"
		arguments = append(arguments, status)
	}
	if limit <= 0 {
		limit = 100
	}
	arguments = append(arguments, limit, offset)
	ids, err := stringColumn(s.db, `SELECT id FROM photo_reviews WHERE `+conditions+` ORDER BY seq DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, err
	}
	reviews := make([]PhotoReview, 0, len(ids))
	for _, id := range ids {
		review, err := photoReviewRow(s.db, id)
		if err != nil {
			return nil, err
		}
		if err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM model_readings WHERE photo_review_id = ?`, id).Scan(&review.CostUSD); err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

// RetryPhotoReview queues a failed review again. Its readings are kept in
// revisions and cleared, so the readers start fresh.
func (s *Store) RetryPhotoReview(id, actor string) (PhotoReview, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return PhotoReview{}, err
	}
	defer transaction.Rollback()
	review, err := photoReviewRow(transaction, id)
	if err != nil {
		return PhotoReview{}, err
	}
	if review.Status != "failed" {
		return PhotoReview{}, fmt.Errorf("%w: %s is %s; only a failed review is retried", ErrConflict, id, review.Status)
	}
	now := s.unixNow()
	if err := saveRevision(transaction, "photo_reviews", "id", id, actor, now); err != nil {
		return PhotoReview{}, err
	}
	readings, err := stringColumn(transaction, `SELECT json_object('round', round, 'model_role', model_role, 'model_id', model_id, 'answer', json(answer), 'cost_usd', cost_usd, 'read_at', read_at) FROM model_readings WHERE photo_review_id = ?`, id)
	if err != nil {
		return PhotoReview{}, err
	}
	for _, reading := range readings {
		if _, err := transaction.Exec(`INSERT INTO revisions (record_id, record, changed_by, changed_at) VALUES (?, ?, ?, ?)`, id, `{"table":"model_readings","reading":`+reading+`}`, actor, now); err != nil {
			return PhotoReview{}, err
		}
	}
	if _, err := transaction.Exec(`DELETE FROM model_readings WHERE photo_review_id = ?`, id); err != nil {
		return PhotoReview{}, err
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'queued', error = '', finished_at = 0 WHERE id = ?`, id); err != nil {
		return PhotoReview{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PhotoReview{}, err
	}
	return s.GetPhotoReview(id)
}

func (s *Store) setReviewStatus(id, status, imageSource, failure string) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	finishedAt := int64(0)
	if status == "failed" || status == "done" || status == "waiting_for_owner" {
		finishedAt = s.unixNow()
	}
	_, err := s.db.Exec(`UPDATE photo_reviews SET status = ?, image_source = CASE WHEN ? = '' THEN image_source ELSE ? END, error = ?, finished_at = ? WHERE id = ?`,
		status, imageSource, imageSource, failure, finishedAt, id)
	return err
}

func (s *Store) saveReading(reviewID string, round int, role string, answer ModelAnswer) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	_, err := s.db.Exec(`INSERT INTO model_readings (photo_review_id, round, model_role, model_id, answer, input_tokens, output_tokens, cost_usd, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reviewID, round, role, answer.Model, string(answer.Parsed), answer.InputTokens, answer.OutputTokens, answer.CostUSD, s.unixNow())
	return err
}

// RequeueInterrupted puts back in the queue any review a stopped service left
// half done; its readings are cleared, as on a retry.
func (s *Store) RequeueInterrupted() error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`DELETE FROM model_readings WHERE photo_review_id IN (SELECT id FROM photo_reviews WHERE status IN ('reading', 'reconciling'))`); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'queued' WHERE status IN ('reading', 'reconciling')`); err != nil {
		return err
	}
	return transaction.Commit()
}

// Run reviews queued photos one at a time until ctx ends.
func (reviewer *Reviewer) Run(ctx context.Context) {
	if err := reviewer.Store.RequeueInterrupted(); err != nil {
		log.Printf("review worker: requeue interrupted reviews: %v", err)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		for {
			var id string
			err := reviewer.Store.db.QueryRow(`SELECT id FROM photo_reviews WHERE status = 'queued' ORDER BY seq LIMIT 1`).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				log.Printf("review worker: read the queue: %v", err)
				break
			}
			if err := reviewer.Review(ctx, id); err != nil {
				log.Printf("review worker: %s failed: %v", id, err)
				if statusErr := reviewer.Store.setReviewStatus(id, "failed", "", err.Error()); statusErr != nil {
					log.Printf("review worker: record that %s failed: %v", id, statusErr)
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-reviewer.wakeChannel():
		}
	}
}

type readerResult struct {
	answer ModelAnswer
	err    error
}

// askBoth asks the two readers at once about one photo.
func (reviewer *Reviewer) askBoth(ctx context.Context, prompts [2]string, schema json.RawMessage, image msg.OneShotImage) ([2]ModelAnswer, error) {
	return reviewer.askBothPrompts(ctx, readerSystemPrompt(), prompts, schema, []msg.OneShotImage{image}, 16000)
}

// Review runs one queued review to its end: done, or waiting for the owner.
func (reviewer *Reviewer) Review(ctx context.Context, reviewID string) error {
	store := reviewer.Store
	review, err := photoReviewRow(store.db, reviewID)
	if err != nil {
		return err
	}
	if review.Reading == "outfit" {
		return reviewer.reviewOutfit(ctx, review)
	}
	photo, imageSource, err := reviewer.Photos.ReviewImage(ctx, review.PhotoID)
	if err != nil {
		return err
	}
	if err := store.setReviewStatus(reviewID, "reading", imageSource, ""); err != nil {
		return err
	}
	image := msg.OneShotImage{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(photo)}

	firstPrompt := "Round one. List every possession you see in this photo. Give each a box. Do not try to match anything to an inventory yet."
	firstAnswers, err := reviewer.askBoth(ctx, [2]string{firstPrompt, firstPrompt}, readingSchema(false), image)
	if err != nil {
		return err
	}
	firstReadings := [2]firstReading{}
	roles := [2]string{reviewer.FirstReaderRole, reviewer.SecondReaderRole}
	for index, answer := range firstAnswers {
		if err := store.saveReading(reviewID, 1, roles[index], answer); err != nil {
			return err
		}
		if err := json.Unmarshal(answer.Parsed, &firstReadings[index]); err != nil {
			return fmt.Errorf("%s's round-one answer is not a reading: %w", answer.Model, err)
		}
	}

	if err := store.setReviewStatus(reviewID, "reconciling", "", ""); err != nil {
		return err
	}
	candidates, err := store.candidateList(reviewID)
	if err != nil {
		return err
	}
	secondPrompts := [2]string{
		secondRoundPrompt("A", firstReadings[0].Items, firstReadings[1].Items, candidates),
		secondRoundPrompt("B", firstReadings[0].Items, firstReadings[1].Items, candidates),
	}
	secondAnswers, err := reviewer.askBoth(ctx, secondPrompts, readingSchema(true), image)
	if err != nil {
		return err
	}
	finals := [2]finalReading{}
	for index, answer := range secondAnswers {
		if err := store.saveReading(reviewID, 2, roles[index], answer); err != nil {
			return err
		}
		if err := json.Unmarshal(answer.Parsed, &finals[index]); err != nil {
			return fmt.Errorf("%s's round-two answer is not a reading: %w", answer.Model, err)
		}
	}
	readerNames := [2]string{secondAnswers[0].Model, secondAnswers[1].Model}
	return store.recordReconciliation(ctx, reviewID, review.PhotoID, readerNames, finals)
}

// matchedPair is one thing in the photo as each reader's final list has it;
// a nil side means that reader did not list it.
type matchedPair struct {
	items [2]*finalItem
}

// matchFinalLists pairs the two final lists: first by the round-one labels
// they share, then, for entries that share none, by boxes that overlap.
func matchFinalLists(finals [2]finalReading) []matchedPair {
	usedSecond := map[int]bool{}
	pairs := []matchedPair{}
	for firstIndex := range finals[0].Items {
		first := &finals[0].Items[firstIndex]
		bestIndex, bestScore := -1, 0.0
		for secondIndex := range finals[1].Items {
			if usedSecond[secondIndex] {
				continue
			}
			second := &finals[1].Items[secondIndex]
			score := 0.0
			for _, label := range first.FromReadings {
				for _, other := range second.FromReadings {
					if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(other)) {
						score += 1
					}
				}
			}
			overlap := first.Box.clamped().overlap(second.Box.clamped())
			if score == 0 && (overlap < 0.5 || first.Kind != second.Kind) {
				continue
			}
			score += overlap
			if score > bestScore {
				bestIndex, bestScore = secondIndex, score
			}
		}
		pair := matchedPair{items: [2]*finalItem{first, nil}}
		if bestIndex >= 0 {
			usedSecond[bestIndex] = true
			pair.items[1] = &finals[1].Items[bestIndex]
		}
		pairs = append(pairs, pair)
	}
	for secondIndex := range finals[1].Items {
		if !usedSecond[secondIndex] {
			pairs = append(pairs, matchedPair{items: [2]*finalItem{nil, &finals[1].Items[secondIndex]}})
		}
	}
	return pairs
}

func normalizedWords(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// versionFor turns a reader's final entry into its proposal, splitting its
// existing_id into a possession or an earlier sighting.
func (s *Store) versionFor(item *finalItem) Proposal {
	proposal := normalizeProposal(item.readerItem)
	existing := strings.TrimSpace(item.ExistingID)
	switch {
	case strings.HasPrefix(existing, "possession_"):
		if requireLivePossession(s.db, existing) == nil {
			proposal.ExistingPossessionID = existing
		} else {
			proposal.DroppedFields = append(proposal.DroppedFields, "existing_id "+existing+" (no such possession)")
		}
	case strings.HasPrefix(existing, "sighting_"):
		var found int
		if s.db.QueryRow(`SELECT 1 FROM sightings WHERE id = ?`, existing).Scan(&found) == nil {
			proposal.ExistingSightingID = existing
		} else {
			proposal.DroppedFields = append(proposal.DroppedFields, "existing_id "+existing+" (no such sighting)")
		}
	case existing != "":
		proposal.DroppedFields = append(proposal.DroppedFields, "existing_id "+existing+" (not a possession or sighting id)")
	}
	return proposal
}

// differences lists what two versions disagree on, as lines for a question.
// Empty means they agree. A field one side left empty is not a difference.
func differences(names [2]string, versions [2]Proposal) []string {
	lines := []string{}
	differ := func(label, first, second string) {
		if first != "" && second != "" && normalizedWords(first) != normalizedWords(second) {
			lines = append(lines, fmt.Sprintf("%s: %q (%s) or %q (%s)", label, first, names[0], second, names[1]))
		}
	}
	if versions[0].Kind != versions[1].Kind {
		lines = append(lines, fmt.Sprintf("kind: %q (%s) or %q (%s)", versions[0].Kind, names[0], versions[1].Kind, names[1]))
	}
	differ("brand", versions[0].Brand, versions[1].Brand)
	differ("model", versions[0].Model, versions[1].Model)
	if versions[0].ExistingPossessionID != versions[1].ExistingPossessionID || versions[0].ExistingSightingID != versions[1].ExistingSightingID {
		describe := func(version Proposal) string {
			switch {
			case version.ExistingPossessionID != "":
				return "the same as " + version.ExistingPossessionID
			case version.ExistingSightingID != "":
				return "the same as " + version.ExistingSightingID + " from an earlier photo"
			}
			return "new"
		}
		lines = append(lines, fmt.Sprintf("is it new? %s says %s; %s says %s", names[0], describe(versions[0]), names[1], describe(versions[1])))
	}
	if versions[0].Kind == versions[1].Kind {
		if kind, known := KindByName(versions[0].Kind); known {
			for _, column := range kind.Columns {
				if column.Type == ColumnText && len(column.Vocabulary) > 0 {
					differ(column.Name, strings.Trim(string(versions[0].KindFields[column.Name]), `"`), strings.Trim(string(versions[1].KindFields[column.Name]), `"`))
				}
			}
		}
	}
	if versions[0].Quantity != versions[1].Quantity {
		lines = append(lines, fmt.Sprintf("how many: %d (%s) or %d (%s)", versions[0].Quantity, names[0], versions[1].Quantity, names[1]))
	}
	return lines
}

// merged fills the gaps in the first version from the second.
func merged(versions [2]Proposal) Proposal {
	result := versions[0]
	if result.Brand == "" {
		result.Brand = versions[1].Brand
	}
	if result.Model == "" {
		result.Model = versions[1].Model
	}
	if result.Description == "" {
		result.Description = versions[1].Description
	}
	fields := map[string]json.RawMessage{}
	for name, value := range versions[1].KindFields {
		fields[name] = value
	}
	for name, value := range versions[0].KindFields {
		fields[name] = value
	}
	result.KindFields = fields
	result.DroppedFields = append(append([]string{}, versions[0].DroppedFields...), versions[1].DroppedFields...)
	return result
}

func averageBox(first, second Box) Box {
	return Box{(first.Left + second.Left) / 2, (first.Top + second.Top) / 2, (first.Right + second.Right) / 2, (first.Bottom + second.Bottom) / 2}.clamped()
}

func versionChoice(label string, version Proposal) Choice {
	switch {
	case version.ExistingPossessionID != "":
		return Choice{Label: label + ": it is " + version.ExistingPossessionID, Resolution: Resolution{Action: "same_as", PossessionID: version.ExistingPossessionID}}
	case version.ExistingSightingID != "":
		return Choice{Label: label + ": it is the thing in " + version.ExistingSightingID + " from an earlier photo", Resolution: Resolution{Action: "same_as_sighting", SightingID: version.ExistingSightingID}}
	}
	copied := version
	return Choice{Label: label + ": " + version.summary(), Resolution: Resolution{Action: "accept", Proposal: &copied}}
}

var rejectChoice = Choice{Label: "Not mine, or not worth recording", Resolution: Resolution{Action: "reject"}}

// keepChoice answers a detail question by leaving the record as it is.
var keepChoice = Choice{Label: "Leave it as recorded", Resolution: Resolution{Action: "keep"}}

// recordReconciliation writes the sightings and questions of one review and
// records what both readers agreed on, all in one transaction.
func (s *Store) recordReconciliation(ctx context.Context, reviewID, photoID string, readerNames [2]string, finals [2]finalReading) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	openQuestions := 0
	for _, pair := range matchFinalLists(finals) {
		var versions [2]Proposal
		var box Box
		var label string
		var doubts []string
		present := 0
		for side, item := range pair.items {
			if item == nil {
				continue
			}
			present++
			versions[side] = s.versionFor(item)
			if label == "" {
				label = strings.TrimSpace(item.Label)
			}
			if doubt := strings.TrimSpace(item.Doubt); doubt != "" {
				doubts = append(doubts, fmt.Sprintf("%s asks: %s", readerNames[side], doubt))
			}
		}
		sighting := Sighting{Label: label, Status: "proposed", Versions: map[string]Proposal{}}
		var choices []Choice
		var question string
		purpose := "decision"
		if present == 2 {
			box = averageBox(pair.items[0].Box.clamped(), pair.items[1].Box.clamped())
			disagreements := differences(readerNames, versions)
			if len(disagreements) == 0 {
				sighting.Agreement = "both_agree"
				sighting.Proposed = merged(versions)
				if len(doubts) > 0 {
					purpose = "detail"
					question = fmt.Sprintf("Recorded as %s. To add: %s", sighting.Proposed.summary(), strings.Join(doubts, " "))
					choices = []Choice{keepChoice}
				}
			} else {
				sighting.Agreement = "readers_differ"
				sighting.Proposed = versions[0]
				for side := range versions {
					sighting.Versions[readerNames[side]] = versions[side]
				}
				question = fmt.Sprintf("The two readers differ about the %s. %s.", label, strings.Join(disagreements, "; "))
				if len(doubts) > 0 {
					question += " " + strings.Join(doubts, " ")
				}
				choices = []Choice{versionChoice("As "+readerNames[0]+" says", versions[0]), versionChoice("As "+readerNames[1]+" says", versions[1]), rejectChoice}
			}
		} else {
			side := 0
			if pair.items[0] == nil {
				side = 1
			}
			box = pair.items[side].Box.clamped()
			sighting.Agreement = "one_reader_only"
			sighting.Proposed = versions[side]
			sighting.Versions[readerNames[side]] = versions[side]
			question = fmt.Sprintf("Only %s saw this: %s. Is it something you own?", readerNames[side], versions[side].summary())
			if len(doubts) > 0 {
				question += " " + strings.Join(doubts, " ")
			}
			choices = []Choice{versionChoice("Yes", versions[side]), rejectChoice}
		}
		if !box.valid() {
			box = Box{0, 0, 1, 1}
		}
		sighting.Box = box
		sightingID, sequence, err := nextID(transaction, "sightings", "sighting")
		if err != nil {
			return err
		}
		proposed, _ := json.Marshal(sighting.Proposed)
		versionsJSON, _ := json.Marshal(sighting.Versions)
		if _, err := transaction.Exec(`INSERT INTO sightings (id, seq, photo_review_id, label, box_left, box_top, box_right, box_bottom, proposed, versions, agreement, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'proposed')`,
			sightingID, sequence, reviewID, label, box.Left, box.Top, box.Right, box.Bottom, string(proposed), string(versionsJSON), sighting.Agreement); err != nil {
			return err
		}
		if question != "" && purpose == "decision" {
			if err := insertQuestion(transaction, sightingID, question, choices, purpose, now); err != nil {
				return err
			}
			openQuestions++
			continue
		}
		// Both readers agree: record it, and ask any detail they wanted.
		resolution := versionChoice("", sighting.Proposed).Resolution
		if err := s.applyResolution(ctx, transaction, sightingID, resolution, "agreement", reviewID, now); err != nil {
			return fmt.Errorf("record %s: %w", sightingID, err)
		}
		if question != "" {
			if err := insertQuestion(transaction, sightingID, question, choices, purpose, now); err != nil {
				return err
			}
		}
	}
	status := "done"
	if openQuestions > 0 {
		status = "waiting_for_owner"
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = ?, finished_at = ? WHERE id = ?`, status, now, reviewID); err != nil {
		return err
	}
	return transaction.Commit()
}

func insertQuestion(transaction *sql.Tx, sightingID, question string, choices []Choice, purpose string, now int64) error {
	_, err := insertQuestionAnsweringID(transaction, sightingID, question, choices, purpose, now)
	return err
}

func insertQuestionAnsweringID(transaction *sql.Tx, sightingID, question string, choices []Choice, purpose string, now int64) (string, error) {
	questionID, sequence, err := nextID(transaction, "owner_questions", "possession_question")
	if err != nil {
		return "", err
	}
	encoded, _ := json.Marshal(choices)
	_, err = transaction.Exec(`INSERT INTO owner_questions (id, seq, sighting_id, question, choices, purpose, asked_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, questionID, sequence, sightingID, question, string(encoded), purpose, now)
	return questionID, err
}

// applyResolution settles a sighting, then every later sighting that waits
// on it. creator is who a new possession is recorded as made by.
func (s *Store) applyResolution(ctx context.Context, transaction *sql.Tx, sightingID string, resolution Resolution, decidedBy, creator string, now int64) error {
	sighting, err := sightingByID(transaction, sightingID)
	if err != nil {
		return err
	}
	if resolution.UnlinkPossessionID != "" {
		if resolution.Action != "accept" && resolution.Action != "same_as" && resolution.Action != "reject" {
			return fmt.Errorf("%w: unlink_possession_id goes with accept, same_as or reject, not %q", ErrInvalid, resolution.Action)
		}
		if err := s.unlinkSighting(transaction, sighting, resolution.UnlinkPossessionID, decidedBy, now); err != nil {
			return err
		}
		resolution.UnlinkPossessionID = ""
		return s.applyResolution(ctx, transaction, sightingID, resolution, decidedBy, creator, now)
	}
	if sighting.Status != "proposed" && sighting.Status != "same_as_other_sighting" {
		return fmt.Errorf("%w: %s is already %s", ErrConflict, sightingID, sighting.Status)
	}
	setStatus := func(status string, possessionID, sameAs any) error {
		_, err := transaction.Exec(`UPDATE sightings SET status = ?, possession_id = ?, same_as_sighting_id = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
			status, possessionID, sameAs, decidedBy, now, sightingID)
		return err
	}
	link := func(possessionID string) error {
		box := sighting.Box
		return s.linkPhotoInTransaction(ctx, transaction, possessionID, PhotoLink{PhotoID: sighting.PhotoID, BoxLeft: &box.Left, BoxTop: &box.Top, BoxRight: &box.Right, BoxBottom: &box.Bottom,
			SightingID: &sightingID, LinkedBy: decidedBy}, now)
	}
	switch resolution.Action {
	case "accept":
		if resolution.Proposal == nil {
			return fmt.Errorf("%w: accept needs a proposal", ErrInvalid)
		}
		if group := proposalGroup(*resolution.Proposal); group != "" {
			closed, err := groupClosed(transaction, group)
			if err != nil {
				return err
			}
			if closed {
				return fmt.Errorf("%w: the owner closed the %s group, so nothing in it is recorded as new; pick one of its members or not mine", ErrConflict, group)
			}
		}
		possessionID, err := s.createPossessionInTransaction(ctx, transaction, resolution.Proposal.possessionFields(), creator)
		if err != nil {
			return err
		}
		if err := link(possessionID); err != nil {
			return err
		}
		if err := setStatus("accepted", possessionID, nil); err != nil {
			return err
		}
		if resolution.Proposal.Status == "unconfirmed" {
			if err := insertStillHaveItQuestion(transaction, sightingID, resolution.Proposal.Name, now); err != nil {
				return err
			}
		}
	case "same_as":
		if err := requireLivePossession(transaction, resolution.PossessionID); err != nil {
			return err
		}
		if err := link(resolution.PossessionID); err != nil {
			return err
		}
		if err := setStatus("accepted", resolution.PossessionID, nil); err != nil {
			return err
		}
	case "same_as_sighting":
		target, err := sightingByID(transaction, resolution.SightingID)
		if err != nil {
			return err
		}
		if target.ID == sightingID {
			return fmt.Errorf("%w: a sighting cannot wait on itself", ErrInvalid)
		}
		switch target.Status {
		case "accepted":
			return s.applyResolution(ctx, transaction, sightingID, Resolution{Action: "same_as", PossessionID: *target.PossessionID}, decidedBy, creator, now)
		case "rejected":
			return s.applyResolution(ctx, transaction, sightingID, Resolution{Action: "reject"}, decidedBy, creator, now)
		}
		return setStatus("same_as_other_sighting", nil, target.ID)
	case "reject":
		if err := setStatus("rejected", nil, nil); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: action %q is not one of accept, same_as, same_as_sighting, reject", ErrInvalid, resolution.Action)
	}
	// Settle the sightings that were waiting on this one.
	waiting, err := stringColumn(transaction, `SELECT id FROM sightings WHERE same_as_sighting_id = ? AND status = 'same_as_other_sighting'`, sightingID)
	if err != nil {
		return err
	}
	for _, waitingID := range waiting {
		followOn := Resolution{Action: "reject"}
		if resolution.Action != "reject" {
			settled, err := sightingByID(transaction, sightingID)
			if err != nil {
				return err
			}
			followOn = Resolution{Action: "same_as", PossessionID: *settled.PossessionID}
		}
		if err := s.applyResolution(ctx, transaction, waitingID, followOn, decidedBy, creator, now); err != nil {
			return err
		}
	}
	return s.addToReviewOutfit(transaction, sightingID, now)
}

func sightingByID(q querier, id string) (Sighting, error) {
	var sighting Sighting
	var proposed, versions string
	var sameAs, possessionID sql.NullString
	err := q.QueryRow(`SELECT sightings.id, sightings.photo_review_id, photo_reviews.photo_id, sightings.label, box_left, box_top, box_right, box_bottom, proposed, versions, agreement,
		sightings.status, same_as_sighting_id, possession_id, decided_by, decided_at FROM sightings JOIN photo_reviews ON photo_reviews.id = sightings.photo_review_id WHERE sightings.id = ?`, id).Scan(
		&sighting.ID, &sighting.PhotoReviewID, &sighting.PhotoID, &sighting.Label, &sighting.Box.Left, &sighting.Box.Top, &sighting.Box.Right, &sighting.Box.Bottom, &proposed, &versions,
		&sighting.Agreement, &sighting.Status, &sameAs, &possessionID, &sighting.DecidedBy, &sighting.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Sighting{}, fmt.Errorf("%w: no sighting %s", ErrNotFound, id)
	}
	if err != nil {
		return Sighting{}, err
	}
	if err := json.Unmarshal([]byte(proposed), &sighting.Proposed); err != nil {
		return Sighting{}, fmt.Errorf("%s holds a proposal that is not JSON: %w", id, err)
	}
	if err := json.Unmarshal([]byte(versions), &sighting.Versions); err != nil {
		return Sighting{}, fmt.Errorf("%s holds versions that are not JSON: %w", id, err)
	}
	if sameAs.Valid {
		sighting.SameAsSightingID = &sameAs.String
	}
	if possessionID.Valid {
		sighting.PossessionID = &possessionID.String
	}
	if sighting.QuestionIDs, err = stringColumn(q, `SELECT id FROM owner_questions WHERE sighting_id = ? ORDER BY seq`, id); err != nil {
		return Sighting{}, err
	}
	return sighting, nil
}

// GetSighting answers one sighting.
func (s *Store) GetSighting(id string) (Sighting, error) { return sightingByID(s.db, id) }

// ---------------------------------------------------------------------------
// Questions.
// ---------------------------------------------------------------------------

func questionByID(q querier, id string) (OwnerQuestion, error) {
	var question OwnerQuestion
	var choices, resolution string
	var answerChoice sql.NullInt64
	err := q.QueryRow(`SELECT owner_questions.id, sighting_id, photo_reviews.photo_id, owner_questions.purpose, owner_questions.asked_by, question, choices, answer_choice, answer_text, resolution, answered_by, asked_at, answered_at
		FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id JOIN photo_reviews ON photo_reviews.id = sightings.photo_review_id WHERE owner_questions.id = ?`, id).Scan(
		&question.ID, &question.SightingID, &question.PhotoID, &question.Purpose, &question.AskedBy, &question.Question, &choices, &answerChoice, &question.AnswerText, &resolution, &question.AnsweredBy, &question.AskedAt, &question.AnsweredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerQuestion{}, fmt.Errorf("%w: no question %s", ErrNotFound, id)
	}
	if err != nil {
		return OwnerQuestion{}, err
	}
	if err := json.Unmarshal([]byte(choices), &question.Choices); err != nil {
		return OwnerQuestion{}, fmt.Errorf("%s holds choices that are not JSON: %w", id, err)
	}
	if answerChoice.Valid {
		index := int(answerChoice.Int64)
		question.AnswerChoice = &index
	}
	if resolution != "" {
		question.Resolution = json.RawMessage(resolution)
	}
	return question, nil
}

// GetQuestion answers one question.
func (s *Store) GetQuestion(id string) (OwnerQuestion, error) { return questionByID(s.db, id) }

// ListQuestions answers questions, oldest first; open only unless all.
func (s *Store) ListQuestions(includeAnswered bool, photoReviewID, purpose string, limit, offset int) ([]OwnerQuestion, error) {
	conditions := []string{"1 = 1"}
	arguments := []any{}
	if purpose != "" {
		if !contains(QuestionPurposes, purpose) {
			return nil, fmt.Errorf("%w: purpose must be one of %s", ErrInvalid, strings.Join(QuestionPurposes, ", "))
		}
		conditions = append(conditions, "owner_questions.purpose = ?")
		arguments = append(arguments, purpose)
	}
	if !includeAnswered {
		conditions = append(conditions, "owner_questions.answered_at = 0")
	}
	if photoReviewID != "" {
		conditions = append(conditions, "sightings.photo_review_id = ?")
		arguments = append(arguments, photoReviewID)
	}
	if limit <= 0 {
		limit = 100
	}
	arguments = append(arguments, limit, offset)
	ids, err := stringColumn(s.db, `SELECT owner_questions.id FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id WHERE `+strings.Join(conditions, " AND ")+
		` ORDER BY owner_questions.seq LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, err
	}
	questions := make([]OwnerQuestion, 0, len(ids))
	for _, id := range ids {
		question, err := questionByID(s.db, id)
		if err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return questions, nil
}

// Answer is the body of POST /owner-questions/{id}/answer: a choice, or the
// owner's own words, never both.
type Answer struct {
	Choice     *int   `json:"choice"`
	Text       string `json:"text"`
	AnsweredBy string `json:"answered_by"`
}

// AnswerQuestion applies an owner's answer. Words go to the first reader,
// which turns them into a resolution, before anything is written.
func (reviewer *Reviewer) AnswerQuestion(ctx context.Context, questionID string, answer Answer) (OwnerQuestion, error) {
	store := reviewer.Store
	if answer.AnsweredBy == "" {
		return OwnerQuestion{}, fmt.Errorf("%w: answered_by is required", ErrInvalid)
	}
	answer.Text = strings.TrimSpace(answer.Text)
	if (answer.Choice == nil) == (answer.Text == "") {
		return OwnerQuestion{}, fmt.Errorf("%w: give a choice or text, not both and not neither", ErrInvalid)
	}
	question, err := questionByID(store.db, questionID)
	if err != nil {
		return OwnerQuestion{}, err
	}
	if question.AnsweredAt != 0 {
		return OwnerQuestion{}, fmt.Errorf("%w: %s is already answered", ErrConflict, questionID)
	}
	var resolution Resolution
	switch {
	case answer.Choice != nil:
		if *answer.Choice < 0 || *answer.Choice >= len(question.Choices) {
			return OwnerQuestion{}, fmt.Errorf("%w: choice is 0 to %d", ErrInvalid, len(question.Choices)-1)
		}
		resolution = question.Choices[*answer.Choice].Resolution
	case question.isAudit():
		resolution, err = reviewer.interpretChoice(ctx, question, answer.Text)
	case question.Purpose == "detail":
		resolution, err = reviewer.interpretDetail(ctx, question, answer.Text)
	default:
		resolution, err = reviewer.interpretAnswer(ctx, question, answer.Text)
	}
	if err != nil {
		return OwnerQuestion{}, err
	}
	store.writeMutex.Lock()
	defer store.writeMutex.Unlock()
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return OwnerQuestion{}, err
	}
	defer transaction.Rollback()
	now := store.unixNow()
	if contains(auditActions, resolution.Action) {
		if err := store.applyAuditResolution(transaction, question, resolution, answer.AnsweredBy, now); err != nil {
			return OwnerQuestion{}, err
		}
	} else if question.Purpose == "detail" {
		if err := store.applyDetail(ctx, transaction, question.SightingID, resolution, answer.AnsweredBy); err != nil {
			return OwnerQuestion{}, err
		}
	} else if err := store.applyResolution(ctx, transaction, question.SightingID, resolution, answer.AnsweredBy, answer.AnsweredBy, now); err != nil {
		return OwnerQuestion{}, err
	}
	encoded, _ := json.Marshal(resolution)
	result, err := transaction.Exec(`UPDATE owner_questions SET answer_choice = ?, answer_text = ?, resolution = ?, answered_by = ?, answered_at = ? WHERE id = ? AND answered_at = 0`,
		answer.Choice, answer.Text, string(encoded), answer.AnsweredBy, now, questionID)
	if err != nil {
		return OwnerQuestion{}, err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return OwnerQuestion{}, fmt.Errorf("%w: %s was answered meanwhile", ErrConflict, questionID)
	}
	// A review is done once none of its questions is open.
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'done', finished_at = ? WHERE status = 'waiting_for_owner' AND id = (SELECT photo_review_id FROM sightings WHERE id = ?)
		AND NOT EXISTS (SELECT 1 FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id
			WHERE sightings.photo_review_id = photo_reviews.id AND owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision')`, now, question.SightingID); err != nil {
		return OwnerQuestion{}, err
	}
	if err := transaction.Commit(); err != nil {
		return OwnerQuestion{}, err
	}
	return questionByID(store.db, questionID)
}

var interpretationSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["action","possession_id","sighting_id","item","explanation"],"properties":{
	"action":{"type":"string","enum":["accept","same_as","same_as_sighting","reject"]},
	"possession_id":{"type":"string","description":"For same_as: the possession it is"},
	"sighting_id":{"type":"string","description":"For same_as_sighting: the earlier sighting it is"},
	"item":` + mustJSON(itemSchema(false)) + `,
	"explanation":{"type":"string","description":"One sentence: what you took the owner to mean"}}}`)

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// interpretAnswer asks the first reader what an answer in words means.
func (reviewer *Reviewer) interpretAnswer(ctx context.Context, question OwnerQuestion, text string) (Resolution, error) {
	sighting, err := sightingByID(reviewer.Store.db, question.SightingID)
	if err != nil {
		return Resolution{}, err
	}
	photo, _, err := reviewer.Photos.ReviewImage(ctx, sighting.PhotoID)
	if err != nil {
		return Resolution{}, err
	}
	crop, err := cropJPEG(photo, sighting.Box)
	if err != nil {
		return Resolution{}, err
	}
	candidates, err := reviewer.Store.candidateList(sighting.PhotoReviewID)
	if err != nil {
		return Resolution{}, err
	}
	versions, _ := json.MarshalIndent(map[string]any{"proposed": sighting.Proposed, "each_reader": sighting.Versions}, "", "  ")
	choices, _ := json.MarshalIndent(question.Choices, "", "  ")
	prompt := fmt.Sprintf(`The picture is the %s, cut from a photo of the owner's home. You and another reader could not settle it, so the owner was asked:

%s

The suggested answers were:
%s

What the readers proposed:
%s

What the inventory already holds:
%s

The owner answered in their own words:
%q

Decide what the answer means. accept records it as a new possession described by item, corrected by what the owner said. same_as means it is a possession already listed (give possession_id). same_as_sighting means it is a thing from an earlier photo (give sighting_id). reject means the owner does not own it or does not want it recorded. Fill item for accept; otherwise fill it from the proposal as it stands.`,
		sighting.Label, question.Question, choices, versions, candidates, text)
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, readerSystemPrompt(), prompt, interpretationSchema,
		[]msg.OneShotImage{{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(crop)}}, 8000)
	if err != nil {
		return Resolution{}, fmt.Errorf("interpret the answer: %w", err)
	}
	var interpretation struct {
		Action       string     `json:"action"`
		PossessionID string     `json:"possession_id"`
		SightingID   string     `json:"sighting_id"`
		Item         readerItem `json:"item"`
		Explanation  string     `json:"explanation"`
	}
	if err := json.Unmarshal(answer.Parsed, &interpretation); err != nil {
		return Resolution{}, fmt.Errorf("%s's interpretation is not one: %w", answer.Model, err)
	}
	resolution := Resolution{Action: interpretation.Action, Explanation: fmt.Sprintf("%s read the answer as: %s", answer.Model, interpretation.Explanation)}
	switch interpretation.Action {
	case "accept":
		proposal := normalizeProposal(interpretation.Item)
		resolution.Proposal = &proposal
	case "same_as":
		resolution.PossessionID = strings.TrimSpace(interpretation.PossessionID)
	case "same_as_sighting":
		resolution.SightingID = strings.TrimSpace(interpretation.SightingID)
	}
	return resolution, nil
}

// applyDetail carries out the answer to a detail question: keep leaves the
// possession as recorded; amend changes it to what the owner said.
func (s *Store) applyDetail(ctx context.Context, transaction *sql.Tx, sightingID string, resolution Resolution, actor string) error {
	switch resolution.Action {
	case "keep":
		return nil
	case "amend":
	default:
		return fmt.Errorf("%w: a detail question takes keep or amend, not %q", ErrInvalid, resolution.Action)
	}
	if resolution.Proposal == nil {
		return fmt.Errorf("%w: amend needs a proposal", ErrInvalid)
	}
	sighting, err := sightingByID(transaction, sightingID)
	if err != nil {
		return err
	}
	if sighting.PossessionID == nil {
		return fmt.Errorf("%w: %s is not recorded yet (it waits on %v); answer that question first", ErrConflict, sightingID, sighting.SameAsSightingID)
	}
	existing, err := possessionView(transaction, *sighting.PossessionID, false)
	if err != nil {
		return err
	}
	proposal := resolution.Proposal
	text := func(value string) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }
	own := map[string]json.RawMessage{}
	for name, value := range map[string]string{"name": proposal.Name, "brand": proposal.Brand, "model": proposal.Model, "description": proposal.Description, "room": proposal.Room, "status": proposal.Status} {
		if value != "" && value != existing[name] {
			own[name] = text(value)
		}
	}
	if proposal.Quantity > 0 && int64(proposal.Quantity) != existing["quantity"] {
		own["quantity"] = json.RawMessage(strconv.Itoa(proposal.Quantity))
	}
	var kind *RecordType
	if proposal.Kind != "" {
		currentKind := existing["kind"].(string)
		if currentKind != "" && currentKind != proposal.Kind {
			return fmt.Errorf("%w: the answer makes %s a %s, but it is recorded as a %s; change its kind by hand", ErrConflict, *sighting.PossessionID, proposal.Kind, currentKind)
		}
		found, _ := KindByName(proposal.Kind)
		kind = &found
	}
	return s.patchPossessionInTransaction(ctx, transaction, *sighting.PossessionID, own, kind, proposal.KindFields, actor)
}

var detailSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["item","explanation"],"properties":{
	"item":` + mustJSON(itemSchema(false)) + `,
	"explanation":{"type":"string","description":"One sentence: what you changed and why"}}}`)

// interpretDetail asks the first reader to turn the owner's answer to a
// detail question into the possession as it should now read.
func (reviewer *Reviewer) interpretDetail(ctx context.Context, question OwnerQuestion, text string) (Resolution, error) {
	sighting, err := sightingByID(reviewer.Store.db, question.SightingID)
	if err != nil {
		return Resolution{}, err
	}
	if sighting.PossessionID == nil {
		return Resolution{}, fmt.Errorf("%w: %s is not recorded yet; answer the question it waits on first", ErrConflict, question.SightingID)
	}
	possession, err := reviewer.Store.GetPossession(*sighting.PossessionID, false)
	if err != nil {
		return Resolution{}, err
	}
	photo, _, err := reviewer.Photos.ReviewImage(ctx, sighting.PhotoID)
	if err != nil {
		return Resolution{}, err
	}
	crop, err := cropJPEG(photo, sighting.Box)
	if err != nil {
		return Resolution{}, err
	}
	record, _ := json.MarshalIndent(possession, "", "  ")
	prompt := fmt.Sprintf(`The picture is the %s, cut from a photo of the owner's home. It is recorded in the inventory as:
%s

The owner was asked:
%s

The owner answered:
%q

Write the item as it should now read, keeping everything the answer does not change and correcting what it does. Keep the kind it has unless it has none.`,
		sighting.Label, record, question.Question, text)
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, readerSystemPrompt(), prompt, detailSchema,
		[]msg.OneShotImage{{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(crop)}}, 8000)
	if err != nil {
		return Resolution{}, fmt.Errorf("interpret the answer: %w", err)
	}
	var interpretation struct {
		Item        readerItem `json:"item"`
		Explanation string     `json:"explanation"`
	}
	if err := json.Unmarshal(answer.Parsed, &interpretation); err != nil {
		return Resolution{}, fmt.Errorf("%s's interpretation is not one: %w", answer.Model, err)
	}
	proposal := normalizeProposal(interpretation.Item)
	return Resolution{Action: "amend", Proposal: &proposal, Explanation: fmt.Sprintf("%s read the answer as: %s", answer.Model, interpretation.Explanation)}, nil
}

// AgreedRecording says what RecordAgreedSightings did.
type AgreedRecording struct {
	Recorded int                 `json:"recorded"`
	Failures []map[string]string `json:"failures"`
}

// RecordAgreedSightings records every sighting both readers agreed on that
// still waits on a question, and turns that question into a detail
// question. Reviews made before agreed sightings were recorded at once held
// them back; this settles them. Each sighting is its own transaction, and a
// failure is reported, not skipped silently.
func (s *Store) RecordAgreedSightings(ctx context.Context, actor string) (AgreedRecording, error) {
	if actor == "" {
		return AgreedRecording{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	rows, err := s.db.Query(`SELECT sightings.id, owner_questions.id, sightings.photo_review_id FROM sightings JOIN owner_questions ON owner_questions.sighting_id = sightings.id
		WHERE sightings.agreement = 'both_agree' AND sightings.status = 'proposed' AND owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision' ORDER BY sightings.seq`)
	if err != nil {
		return AgreedRecording{}, err
	}
	type pending struct{ sightingID, questionID, reviewID string }
	waiting := []pending{}
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.sightingID, &item.questionID, &item.reviewID); err != nil {
			rows.Close()
			return AgreedRecording{}, err
		}
		waiting = append(waiting, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return AgreedRecording{}, err
	}
	result := AgreedRecording{Failures: []map[string]string{}}
	keepChoices, _ := json.Marshal([]Choice{keepChoice})
	for _, item := range waiting {
		err := func() error {
			s.writeMutex.Lock()
			defer s.writeMutex.Unlock()
			transaction, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer transaction.Rollback()
			sighting, err := sightingByID(transaction, item.sightingID)
			if err != nil {
				return err
			}
			if sighting.Status != "proposed" {
				return nil // settled meanwhile by one it waited on
			}
			now := s.unixNow()
			if err := s.applyResolution(ctx, transaction, item.sightingID, versionChoice("", sighting.Proposed).Resolution, "agreement", item.reviewID, now); err != nil {
				return err
			}
			if err := saveRevision(transaction, "owner_questions", "id", item.questionID, actor, now); err != nil {
				return err
			}
			var question string
			if err := transaction.QueryRow(`SELECT question FROM owner_questions WHERE id = ?`, item.questionID).Scan(&question); err != nil {
				return err
			}
			question = strings.Replace(strings.Replace(question, "Both readers agree this is ", "Recorded as ", 1), ", but: ", ". To add: ", 1)
			if _, err := transaction.Exec(`UPDATE owner_questions SET purpose = 'detail', question = ?, choices = ? WHERE id = ?`, question, string(keepChoices), item.questionID); err != nil {
				return err
			}
			if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'done', finished_at = ? WHERE id = ? AND status = 'waiting_for_owner'
				AND NOT EXISTS (SELECT 1 FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id
					WHERE sightings.photo_review_id = photo_reviews.id AND owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision')`, now, item.reviewID); err != nil {
				return err
			}
			return transaction.Commit()
		}()
		if err != nil {
			result.Failures = append(result.Failures, map[string]string{"sighting_id": item.sightingID, "error": err.Error()})
			continue
		}
		result.Recorded++
	}
	return result, nil
}

// SightingImage cuts a sighting out of the photo the readers were shown.
func (reviewer *Reviewer) SightingImage(ctx context.Context, sightingID string) ([]byte, error) {
	sighting, err := sightingByID(reviewer.Store.db, sightingID)
	if err != nil {
		return nil, err
	}
	photo, err := reviewer.Photos.Thumbnail(ctx, sighting.PhotoID)
	if err != nil {
		return nil, err
	}
	return cropJPEG(photo, sighting.Box)
}

// PossessionImage cuts a possession out of its cover photo; with no box it
// is the whole thumbnail.
func (reviewer *Reviewer) PossessionImage(ctx context.Context, possessionID string) ([]byte, error) {
	photos, err := possessionPhotos(reviewer.Store.db, possessionID)
	if err != nil {
		return nil, err
	}
	if err := requireLivePossession(reviewer.Store.db, possessionID); err != nil {
		var deletedAt int64
		if reviewer.Store.db.QueryRow(`SELECT deleted_at FROM possessions WHERE id = ?`, possessionID).Scan(&deletedAt) != nil {
			return nil, err
		}
	}
	for _, photo := range photos {
		if !photo.IsCover {
			continue
		}
		thumbnail, err := reviewer.Photos.Thumbnail(ctx, photo.PhotoID)
		if err != nil {
			return nil, err
		}
		if photo.BoxLeft == nil {
			return thumbnail, nil
		}
		return cropJPEG(thumbnail, Box{*photo.BoxLeft, *photo.BoxTop, *photo.BoxRight, *photo.BoxBottom})
	}
	return nil, fmt.Errorf("%w: %s has no photo", ErrNotFound, possessionID)
}
