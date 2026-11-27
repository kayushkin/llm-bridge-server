package possessions

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ColumnType is how a column's value travels in JSON and sits in SQLite.
type ColumnType string

const (
	ColumnText       ColumnType = "text"
	ColumnInteger    ColumnType = "integer"
	ColumnReal       ColumnType = "real"
	ColumnStringList ColumnType = "string_list" // a JSON array of strings, stored as its JSON text
)

// ReferenceKind names the record another column points at, so a write can
// check that the record exists before it stores the id.
type ReferenceKind string

const (
	ReferencePlace          ReferenceKind = "place"           // location-store place_…
	ReferencePhoto          ReferenceKind = "photo"           // photo-store photo_…
	ReferenceHarnessMachine ReferenceKind = "harness_machine" // harness-store machines.id
	// ReferenceMusicInstrument is music-practice-store's music_instrument_….
	// It is stored unchecked: music-practice-store answers only requests that
	// come through dash signed in.
	ReferenceMusicInstrument   ReferenceKind = "music_instrument"
	ReferenceComputer          ReferenceKind = "computer"           // this store's computers
	ReferencePurchaseLine      ReferenceKind = "purchase_line"      // this store's purchase_lines
	ReferencePossession        ReferenceKind = "possession"         // this store's possessions
	ReferenceProduct           ReferenceKind = "product"            // this store's products
	ReferenceArticle           ReferenceKind = "article"            // article-store article_…
	ReferenceBudgetTransaction ReferenceKind = "budget_transaction" // budget-store transaction_…
)

// Column is one field a client may write.
type Column struct {
	Name       string        `json:"name"`
	Type       ColumnType    `json:"type"`
	Vocabulary []string      `json:"vocabulary,omitempty"` // for text: the value; for a list: each item. '' is allowed unless Required
	Required   bool          `json:"required,omitempty"`   // must be given, not empty, on create
	Nullable   bool          `json:"nullable,omitempty"`   // absent or empty is stored as NULL
	Minimum    *int64        `json:"minimum,omitempty"`
	Maximum    *int64        `json:"maximum,omitempty"`
	References ReferenceKind `json:"references,omitempty"`
}

// RecordType is a table whose rows clients write through the generic code.
type RecordType struct {
	Name    string   `json:"name"`   // garment
	Table   string   `json:"table"`  // garments
	Prefix  string   `json:"prefix"` // garment → garment_000001
	Route   string   `json:"route"`  // garments → GET /garments/{id}
	Columns []Column `json:"columns"`
}

func bound(value int64) *int64 { return &value }

var possessionType = RecordType{Name: "possession", Table: "possessions", Prefix: "possession", Route: "possessions", Columns: []Column{
	{Name: "name", Type: ColumnText, Required: true},
	{Name: "brand", Type: ColumnText},
	{Name: "model", Type: ColumnText},
	{Name: "serial_number", Type: ColumnText},
	{Name: "description", Type: ColumnText},
	{Name: "quantity", Type: ColumnInteger, Minimum: bound(1)},
	{Name: "condition", Type: ColumnText, Vocabulary: Conditions},
	{Name: "status", Type: ColumnText, Vocabulary: PossessionStatuses},
	{Name: "place_id", Type: ColumnText, Nullable: true, References: ReferencePlace},
	{Name: "room", Type: ColumnText, Vocabulary: Rooms},
	{Name: "spot", Type: ColumnText},
	{Name: "acquired_at", Type: ColumnInteger, Minimum: bound(0)},
	{Name: "acquired_how", Type: ColumnText, Vocabulary: AcquisitionMethods},
	{Name: "purchase_line_id", Type: ColumnText, Nullable: true, References: ReferencePurchaseLine},
	{Name: "product_id", Type: ColumnText, Nullable: true, References: ReferenceProduct},
	{Name: "budget_transaction_id", Type: ColumnText, Nullable: true, References: ReferenceBudgetTransaction},
	{Name: "price_cents", Type: ColumnInteger, Nullable: true, Minimum: bound(0)},
	{Name: "price_currency", Type: ColumnText},
	{Name: "disposed_at", Type: ColumnInteger, Minimum: bound(0)},
	{Name: "tags", Type: ColumnStringList},
}}

// KindTypes are the kinds a possession may have, one table each.
var KindTypes = []RecordType{
	{Name: "garment", Table: "garments", Prefix: "garment", Route: "garments", Columns: []Column{
		{Name: "category", Type: ColumnText, Required: true, Vocabulary: GarmentCategories},
		{Name: "garment_type", Type: ColumnText, Vocabulary: GarmentTypes},
		{Name: "layer", Type: ColumnText, Vocabulary: GarmentLayers},
		{Name: "primary_color", Type: ColumnText, Vocabulary: Colors},
		{Name: "secondary_colors", Type: ColumnStringList, Vocabulary: Colors},
		{Name: "pattern", Type: ColumnText, Vocabulary: Patterns},
		{Name: "materials", Type: ColumnStringList},
		{Name: "size_label", Type: ColumnText},
		{Name: "size_system", Type: ColumnText, Vocabulary: SizeSystems},
		{Name: "fit", Type: ColumnText, Vocabulary: Fits},
		{Name: "formality", Type: ColumnInteger, Nullable: true, Minimum: bound(1), Maximum: bound(5)},
		{Name: "warmth", Type: ColumnInteger, Nullable: true, Minimum: bound(1), Maximum: bound(5)},
		{Name: "seasons", Type: ColumnStringList, Vocabulary: Seasons},
	}},
	{Name: "jewelry", Table: "jewelry", Prefix: "jewelry", Route: "jewelry", Columns: []Column{
		{Name: "jewelry_type", Type: ColumnText, Required: true, Vocabulary: JewelryTypes},
		{Name: "metals", Type: ColumnStringList, Vocabulary: Metals},
		{Name: "stones", Type: ColumnStringList},
	}},
	{Name: "watch", Table: "watches", Prefix: "watch", Route: "watches", Columns: []Column{
		{Name: "movement", Type: ColumnText, Vocabulary: WatchMovements},
		{Name: "case_diameter_millimeters", Type: ColumnReal, Nullable: true},
		{Name: "dial_color", Type: ColumnText, Vocabulary: Colors},
		{Name: "strap", Type: ColumnText},
	}},
	{Name: "eyewear", Table: "eyewear", Prefix: "eyewear", Route: "eyewear", Columns: []Column{
		{Name: "eyewear_type", Type: ColumnText, Required: true, Vocabulary: EyewearTypes},
		{Name: "frame_style", Type: ColumnText},
		{Name: "frame_color", Type: ColumnText, Vocabulary: Colors},
	}},
	{Name: "computer", Table: "computers", Prefix: "computer", Route: "computers", Columns: []Column{
		{Name: "form_factor", Type: ColumnText, Required: true, Vocabulary: ComputerFormFactors},
		{Name: "harness_machine_id", Type: ColumnText, Nullable: true, References: ReferenceHarnessMachine},
		{Name: "processor", Type: ColumnText},
		{Name: "memory_gigabytes", Type: ColumnInteger, Nullable: true, Minimum: bound(0)},
		{Name: "graphics", Type: ColumnText},
		{Name: "operating_system", Type: ColumnText},
	}},
	{Name: "computer_part", Table: "computer_parts", Prefix: "computer_part", Route: "computer-parts", Columns: []Column{
		{Name: "part_type", Type: ColumnText, Required: true, Vocabulary: ComputerPartTypes},
		{Name: "installed_in_computer_id", Type: ColumnText, Nullable: true, References: ReferenceComputer},
		{Name: "capacity_gigabytes", Type: ColumnInteger, Nullable: true, Minimum: bound(0)},
	}},
	{Name: "audio_device", Table: "audio_devices", Prefix: "audio_device", Route: "audio-devices", Columns: []Column{
		{Name: "device_type", Type: ColumnText, Required: true, Vocabulary: AudioDeviceTypes},
		{Name: "connections", Type: ColumnStringList},
		{Name: "powered", Type: ColumnInteger, Nullable: true, Minimum: bound(0), Maximum: bound(1)},
	}},
	{Name: "musical_instrument", Table: "musical_instruments", Prefix: "musical_instrument", Route: "musical-instruments", Columns: []Column{
		{Name: "music_instrument_id", Type: ColumnText, Nullable: true, References: ReferenceMusicInstrument},
		{Name: "string_count", Type: ColumnInteger, Nullable: true, Minimum: bound(0)},
		{Name: "finish", Type: ColumnText},
		{Name: "current_string_set", Type: ColumnText},
		{Name: "strings_changed_at", Type: ColumnInteger, Minimum: bound(0)},
	}},
	{Name: "vehicle", Table: "vehicles", Prefix: "vehicle", Route: "vehicles", Columns: []Column{
		{Name: "vehicle_type", Type: ColumnText, Required: true, Vocabulary: VehicleTypes},
		{Name: "model_year", Type: ColumnInteger, Nullable: true, Minimum: bound(1880), Maximum: bound(2200)},
		{Name: "vehicle_identification_number", Type: ColumnText},
		{Name: "license_plate", Type: ColumnText},
		{Name: "registration_state", Type: ColumnText},
	}},
	{Name: "furniture", Table: "furniture", Prefix: "furniture", Route: "furniture", Columns: []Column{
		{Name: "furniture_type", Type: ColumnText, Required: true, Vocabulary: FurnitureTypes},
		{Name: "materials", Type: ColumnStringList},
		{Name: "primary_color", Type: ColumnText, Vocabulary: Colors},
	}},
	{Name: "fitness_equipment", Table: "fitness_equipment", Prefix: "fitness_equipment", Route: "fitness-equipment", Columns: []Column{
		{Name: "equipment_type", Type: ColumnText, Required: true, Vocabulary: FitnessEquipmentTypes},
		{Name: "weight_pounds", Type: ColumnReal, Nullable: true},
	}},
	{Name: "sports_gear", Table: "sports_gear", Prefix: "sports_gear", Route: "sports-gear", Columns: []Column{
		{Name: "sport", Type: ColumnText, Required: true, Vocabulary: Sports},
		{Name: "gear_type", Type: ColumnText},
		{Name: "size_label", Type: ColumnText},
	}},
	{Name: "tabletop_game", Table: "tabletop_games", Prefix: "tabletop_game", Route: "tabletop-games", Columns: []Column{
		{Name: "game_type", Type: ColumnText, Required: true, Vocabulary: TabletopGameTypes},
		{Name: "title", Type: ColumnText},
		{Name: "publisher", Type: ColumnText},
		{Name: "minimum_players", Type: ColumnInteger, Nullable: true, Minimum: bound(1)},
		{Name: "maximum_players", Type: ColumnInteger, Nullable: true, Minimum: bound(1)},
		{Name: "piece_count", Type: ColumnInteger, Nullable: true, Minimum: bound(1)},
	}},
	{Name: "media", Table: "media", Prefix: "media", Route: "media", Columns: []Column{
		{Name: "media_type", Type: ColumnText, Required: true, Vocabulary: MediaTypes},
		{Name: "title", Type: ColumnText},
		{Name: "creator", Type: ColumnText},
		{Name: "platform_or_format", Type: ColumnText},
		{Name: "edition", Type: ColumnText},
	}},
	{Name: "collectible", Table: "collectibles", Prefix: "collectible", Route: "collectibles", Columns: []Column{
		{Name: "collectible_type", Type: ColumnText, Required: true, Vocabulary: CollectibleTypes},
		{Name: "franchise", Type: ColumnText},
		{Name: "character", Type: ColumnText},
		{Name: "grade", Type: ColumnText},
	}},
	{Name: "art_and_decor", Table: "art_and_decor", Prefix: "art_and_decor", Route: "art-and-decor", Columns: []Column{
		{Name: "decor_type", Type: ColumnText, Required: true, Vocabulary: DecorTypes},
		{Name: "subject", Type: ColumnText},
		{Name: "artist", Type: ColumnText},
		{Name: "framed", Type: ColumnInteger, Nullable: true, Minimum: bound(0), Maximum: bound(1)},
		{Name: "width_inches", Type: ColumnReal, Nullable: true},
		{Name: "height_inches", Type: ColumnReal, Nullable: true},
	}},
	{Name: "home_textile", Table: "home_textiles", Prefix: "home_textile", Route: "home-textiles", Columns: []Column{
		{Name: "textile_type", Type: ColumnText, Required: true, Vocabulary: HomeTextileTypes},
		{Name: "primary_color", Type: ColumnText, Vocabulary: Colors},
		{Name: "materials", Type: ColumnStringList},
	}},
	{Name: "appliance", Table: "appliances", Prefix: "appliance", Route: "appliances", Columns: []Column{
		{Name: "appliance_type", Type: ColumnText, Required: true, Vocabulary: ApplianceTypes},
		{Name: "power_watts", Type: ColumnInteger, Nullable: true, Minimum: bound(0)},
	}},
	{Name: "personal_care", Table: "personal_care", Prefix: "personal_care", Route: "personal-care", Columns: []Column{
		{Name: "care_type", Type: ColumnText, Required: true, Vocabulary: PersonalCareTypes},
		{Name: "powered", Type: ColumnInteger, Nullable: true, Minimum: bound(0), Maximum: bound(1)},
	}},
	{Name: "bag_or_case", Table: "bags_and_cases", Prefix: "bag_or_case", Route: "bags-and-cases", Columns: []Column{
		{Name: "bag_type", Type: ColumnText, Required: true, Vocabulary: BagTypes},
		{Name: "holds", Type: ColumnText},
		{Name: "holds_possession_id", Type: ColumnText, Nullable: true, References: ReferencePossession},
	}},
	{Name: "electronic_accessory", Table: "electronic_accessories", Prefix: "electronic_accessory", Route: "electronic-accessories", Columns: []Column{
		{Name: "accessory_type", Type: ColumnText, Required: true, Vocabulary: ElectronicAccessoryTypes},
		{Name: "connections", Type: ColumnStringList},
	}},
	{Name: "household_tool", Table: "household_tools", Prefix: "household_tool", Route: "household-tools", Columns: []Column{
		{Name: "tool_type", Type: ColumnText, Required: true, Vocabulary: HouseholdToolTypes},
	}},
	{Name: "music_accessory", Table: "music_accessories", Prefix: "music_accessory", Route: "music-accessories", Columns: []Column{
		{Name: "accessory_type", Type: ColumnText, Required: true, Vocabulary: MusicAccessoryTypes},
		{Name: "instrument_possession_id", Type: ColumnText, Nullable: true, References: ReferencePossession},
	}},
}

// KindNames lists the kinds in order.
func KindNames() []string {
	names := make([]string, len(KindTypes))
	for index, kind := range KindTypes {
		names[index] = kind.Name
	}
	return names
}

// KindByName finds a kind; ok is false for an unknown name.
func KindByName(name string) (RecordType, bool) {
	for _, kind := range KindTypes {
		if kind.Name == name {
			return kind, true
		}
	}
	return RecordType{}, false
}

func (recordType RecordType) column(name string) (Column, bool) {
	for _, column := range recordType.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return Column{}, false
}

// decodedValue is one column's value, ready for SQL, and the external id it
// points at, which the caller checks.
type decodedValue struct {
	column Column
	value  any // string, int64, float64 or nil
}

// decodeFields turns a client's JSON object into column values. An unknown
// field, a wrong type, a value outside a vocabulary or a range is an error
// naming the field. When creating, every Required column must be present.
func (recordType RecordType) decodeFields(fields map[string]json.RawMessage, creating bool) (map[string]decodedValue, error) {
	values := map[string]decodedValue{}
	for name, raw := range fields {
		column, known := recordType.column(name)
		if !known {
			return nil, fmt.Errorf("%w: %s has no field %q", ErrInvalid, recordType.Name, name)
		}
		value, err := column.decode(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s.%s: %v", ErrInvalid, recordType.Name, name, err)
		}
		values[name] = decodedValue{column: column, value: value}
	}
	for _, column := range recordType.Columns {
		if !column.Required {
			continue
		}
		decoded, present := values[column.Name]
		if creating && !present {
			return nil, fmt.Errorf("%w: %s.%s is required", ErrInvalid, recordType.Name, column.Name)
		}
		if present && (decoded.value == nil || decoded.value == "") {
			return nil, fmt.Errorf("%w: %s.%s cannot be empty", ErrInvalid, recordType.Name, column.Name)
		}
	}
	return values, nil
}

func (column Column) decode(raw json.RawMessage) (any, error) {
	isNull := strings.TrimSpace(string(raw)) == "null"
	if isNull {
		if column.Nullable {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot be null")
	}
	switch column.Type {
	case ColumnText:
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, fmt.Errorf("must be a string")
		}
		text = strings.TrimSpace(text)
		if text != "" && len(column.Vocabulary) > 0 && !contains(column.Vocabulary, text) {
			return nil, fmt.Errorf("%q is not one of %s", text, strings.Join(column.Vocabulary, ", "))
		}
		if text == "" && column.Nullable {
			return nil, nil
		}
		return text, nil
	case ColumnInteger:
		var number int64
		if err := json.Unmarshal(raw, &number); err != nil {
			return nil, fmt.Errorf("must be a whole number")
		}
		if column.Minimum != nil && number < *column.Minimum {
			return nil, fmt.Errorf("must be at least %d", *column.Minimum)
		}
		if column.Maximum != nil && number > *column.Maximum {
			return nil, fmt.Errorf("must be at most %d", *column.Maximum)
		}
		return number, nil
	case ColumnReal:
		var number float64
		if err := json.Unmarshal(raw, &number); err != nil {
			return nil, fmt.Errorf("must be a number")
		}
		return number, nil
	case ColumnStringList:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("must be an array of strings")
		}
		cleaned := []string{}
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if len(column.Vocabulary) > 0 && !contains(column.Vocabulary, item) {
				return nil, fmt.Errorf("%q is not one of %s", item, strings.Join(column.Vocabulary, ", "))
			}
			cleaned = append(cleaned, item)
		}
		encoded, err := json.Marshal(cleaned)
		if err != nil {
			return nil, err
		}
		return string(encoded), nil
	}
	return nil, fmt.Errorf("column type %q has no decoder", column.Type)
}

// scanRow reads one row of the given columns into a JSON-ready map.
func scanRow(scanner interface{ Scan(...any) error }, columns []Column) (map[string]any, error) {
	holders := make([]any, 0, len(columns))
	for _, column := range columns {
		switch column.Type {
		case ColumnInteger:
			holders = append(holders, new(sql.NullInt64))
		case ColumnReal:
			holders = append(holders, new(sql.NullFloat64))
		default:
			holders = append(holders, new(sql.NullString))
		}
	}
	if err := scanner.Scan(holders...); err != nil {
		return nil, err
	}
	record := map[string]any{}
	for index, column := range columns {
		switch typed := holders[index].(type) {
		case *sql.NullInt64:
			if typed.Valid {
				record[column.Name] = typed.Int64
			} else {
				record[column.Name] = nil
			}
		case *sql.NullFloat64:
			if typed.Valid {
				record[column.Name] = typed.Float64
			} else {
				record[column.Name] = nil
			}
		case *sql.NullString:
			if column.Type == ColumnStringList {
				list := []string{}
				if typed.Valid && typed.String != "" {
					if err := json.Unmarshal([]byte(typed.String), &list); err != nil {
						return nil, fmt.Errorf("%s holds a list that is not JSON: %w", column.Name, err)
					}
				}
				record[column.Name] = list
			} else {
				record[column.Name] = nullableString(typed)
			}
		}
	}
	return record, nil
}

// readColumns are the columns a read returns: the id, the editable columns,
// then the bookkeeping ones.
func (recordType RecordType) readColumns() []Column {
	columns := []Column{{Name: "id", Type: ColumnText}}
	if recordType.Name == possessionType.Name {
		columns = append(columns, Column{Name: "owner_principal_id", Type: ColumnText})
	}
	if recordType.Name != possessionType.Name {
		columns = append(columns, Column{Name: "possession_id", Type: ColumnText})
	}
	columns = append(columns, recordType.Columns...)
	if recordType.Name == possessionType.Name {
		columns = append(columns,
			Column{Name: "created_by", Type: ColumnText},
			Column{Name: "created_at", Type: ColumnInteger},
			Column{Name: "updated_at", Type: ColumnInteger},
			Column{Name: "deleted_at", Type: ColumnInteger},
			Column{Name: "merged_into_id", Type: ColumnText})
	}
	return columns
}

func nullableString(value *sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func columnNames(columns []Column) []string {
	names := make([]string, len(columns))
	for index, column := range columns {
		names[index] = column.Name
	}
	return names
}
