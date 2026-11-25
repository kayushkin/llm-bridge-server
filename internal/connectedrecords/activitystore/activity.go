package activitystore

// The wire types. generate-ts.sh renders this file and vocabulary.go to
// ts/model.ts, so bridge-ui's activity chip reads the same shapes.

// Activity is one thing done at one time: a search, a page visited, a video
// watched, an app used, a place looked up on a map.
type Activity struct {
	ID string `json:"id"` // activity_000001
	// OwnerPrincipalID is principal-store's id of the person whose activity
	// it is.
	OwnerPrincipalID string `json:"owner_principal_id"`
	// Product is Google's name for where it happened (Search, YouTube, Maps,
	// Chrome…), as the export names it.
	Product string `json:"product"`
	// Action is the verb, from vocabulary.go Actions; ActionText is the
	// export's own words for it ("Searched for", "Directions to").
	Action     string `json:"action"`
	ActionText string `json:"action_text"`
	// Title is the query, the page or video title, or the thing used.
	Title      string   `json:"title"`
	URL        string   `json:"url"`
	OccurredAt int64    `json:"occurred_at"` // epoch seconds, UTC
	Latitude   *float64 `json:"latitude"`    // where the user was, when the source recorded it
	Longitude  *float64 `json:"longitude"`
	Details    Details  `json:"details"`
	Source     string   `json:"source"`     // vocabulary.go Sources: the source that first brought it in
	SourceRef  string   `json:"source_ref"` // what an import finds it again by, within its source
	ImportID   string   `json:"import_id"`
	// Sightings is every source record that described this activity, the
	// first included: the same YouTube watch is in My Activity and in YouTube's
	// own history, and is one activity.
	Sightings   []Sighting   `json:"sightings"`
	Attachments []Attachment `json:"attachments"`
	CreatedAt   int64        `json:"created_at"`
	UpdatedAt   int64        `json:"updated_at"`
	DeletedAt   int64        `json:"deleted_at"` // 0 = not
	PurgedAt    int64        `json:"purged_at"`  // 0 = not; a purged activity is also deleted
}

// Details holds what the source said beyond the verb and the title, as
// structured fields. Every field is optional.
type Details struct {
	// Header is the heading of a My Activity card when it is not the product
	// (for Chrome and Ads, the site).
	Header string `json:"header,omitempty"`
	// Products is My Activity's "Products" line.
	Products []string `json:"products,omitempty"`
	// Notes are My Activity's "Details" lines ("From Google Ads").
	Notes []string `json:"notes,omitempty"`
	// Locations are My Activity's "Locations" lines: where the user was when
	// they did it, by the source's account.
	Locations []Location `json:"locations,omitempty"`
	// Channel is the YouTube channel of a video.
	Channel *Link `json:"channel,omitempty"`
	// Lines are the card's further lines, in order: a route's start, an
	// assistant's answer.
	Lines []Line `json:"lines,omitempty"`
	// ChromeClientID and PageTransition come from Chrome's own history.
	ChromeClientID string `json:"chrome_client_id,omitempty"`
	PageTransition string `json:"page_transition,omitempty"`
	// BookmarkFolder is the folder path of a Chrome bookmark.
	BookmarkFolder string `json:"bookmark_folder,omitempty"`
}

// Location is one "Locations" line of a My Activity card.
type Location struct {
	Name      string   `json:"name"` // the link text: an area, a place, or "this place"
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	// Basis is how the source knew: "From your device", "Based on your past
	// activity", "From your places (Home)".
	Basis string `json:"basis"`
	URL   string `json:"url"`
}

// Link is a named link.
type Link struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Line is one further line of a card: its text and the first link in it.
type Line struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

// Sighting is one source record that described an activity.
type Sighting struct {
	Source     string `json:"source"`
	SourceRef  string `json:"source_ref"`
	Product    string `json:"product"`
	Action     string `json:"action"`
	ImportID   string `json:"import_id"`
	OccurredAt int64  `json:"occurred_at"`
	SeenAt     int64  `json:"seen_at"`
}

// Attachment is a file that came with an activity, such as the recording of a
// voice search. file-store keeps its bytes; GET /attachments/{id} serves them.
// It has its activity's owner.
type Attachment struct {
	ID          string `json:"id"` // attachment_000001
	ActivityID  string `json:"activity_id"`
	FileName    string `json:"file_name"` // the name it had in the export
	ContentType string `json:"content_type"`
	Sha256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	CreatedAt   int64  `json:"created_at"`
	PurgedAt    int64  `json:"purged_at"`
}

// Revision is an activity as it was before a delete, restore or purge.
type Revision struct {
	ID               int64    `json:"id"`
	OwnerPrincipalID string   `json:"owner_principal_id"`
	ActivityID       string   `json:"activity_id"`
	Reason           string   `json:"reason"` // vocabulary.go RevisionReasons
	RevisedAt        int64    `json:"revised_at"`
	Activity         Activity `json:"activity"`
}

// Import is one file brought in, found again by its hash.
type Import struct {
	ID               string       `json:"id"` // import_000001
	OwnerPrincipalID string       `json:"owner_principal_id"`
	Kind             string       `json:"kind"`
	Sha256           string       `json:"sha256"`
	SourceMachine    string       `json:"source_machine"`
	SourcePath       string       `json:"source_path"`
	ImportedAt       int64        `json:"imported_at"`
	FinishedAt       int64        `json:"finished_at"` // 0 while it is still sending
	Counts           ImportCounts `json:"counts"`
}

// ImportCounts is what imports did, summed over batches.
type ImportCounts struct {
	Created int `json:"created"`
	// Folded is records that described an activity another source or an
	// earlier export already brought in: they became sightings of it.
	Folded int `json:"folded"`
	// Known is records already imported with the same source reference.
	Known int `json:"known"`
	// AttachmentsStored is files handed to file-store.
	AttachmentsStored int `json:"attachments_stored"`
}

// ImportStart begins an import.
type ImportStart struct {
	Kind          string `json:"kind"`
	Sha256        string `json:"sha256"`
	SourceMachine string `json:"source_machine"`
	SourcePath    string `json:"source_path"`
}

// ImportStarted answers ImportStart.
type ImportStarted struct {
	Import          Import `json:"import"`
	AlreadyFinished bool   `json:"already_finished"`
}

// ImportBatch is one POST of records.
type ImportBatch struct {
	Activities []ImportActivity `json:"activities"`
}

// ImportActivity is one record as an importer read it.
type ImportActivity struct {
	Product     string             `json:"product"`
	Action      string             `json:"action"`
	ActionText  string             `json:"action_text"`
	Title       string             `json:"title"`
	URL         string             `json:"url"`
	OccurredAt  int64              `json:"occurred_at"`
	Latitude    *float64           `json:"latitude"`
	Longitude   *float64           `json:"longitude"`
	Details     Details            `json:"details"`
	Source      string             `json:"source"`
	SourceRef   string             `json:"source_ref"`
	Attachments []ImportAttachment `json:"attachments"`
}

// ImportAttachment is a file sent with a record.
type ImportAttachment struct {
	FileName      string `json:"file_name"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

// Health answers GET /health.
type Health struct {
	Status string       `json:"status"`
	Counts HealthCounts `json:"counts"`
}

// HealthCounts are the store's sizes.
type HealthCounts struct {
	Activities  int `json:"activities"` // live
	Deleted     int `json:"deleted"`
	Purged      int `json:"purged"`
	Sightings   int `json:"sightings"`
	Attachments int `json:"attachments"`
	Imports     int `json:"imports"`
	Revisions   int `json:"revisions"`
}

// Stats answers GET /stats: live activities by product and by source, and
// the span they cover.
type Stats struct {
	Total         int          `json:"total"`
	FirstAt       int64        `json:"first_at"`
	LastAt        int64        `json:"last_at"`
	ByProduct     []NamedCount `json:"by_product"`
	BySource      []NamedCount `json:"by_source"`
	ByAction      []NamedCount `json:"by_action"`
	SightingsFrom []NamedCount `json:"sightings_by_source"`
}

// NamedCount is a count with what it counts.
type NamedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ActivityPage answers GET /activities.
type ActivityPage struct {
	Activities []Activity `json:"activities"`
	Total      int        `json:"total"`
	Limit      int        `json:"limit"`
	Offset     int        `json:"offset"`
}

// Day answers GET /days/{date}: one calendar day in a time zone, by product.
type Day struct {
	Date     string       `json:"date"`     // YYYY-MM-DD
	TimeZone string       `json:"timezone"` // IANA name, as asked
	From     int64        `json:"from"`     // epoch seconds, inclusive
	To       int64        `json:"to"`       // epoch seconds, exclusive
	Total    int          `json:"total"`
	Products []DayProduct `json:"products"` // most activities first
}

// DayProduct is one product's share of a day.
type DayProduct struct {
	Product string       `json:"product"`
	Count   int          `json:"count"`
	FirstAt int64        `json:"first_at"`
	LastAt  int64        `json:"last_at"`
	Actions []NamedCount `json:"actions"` // most first
	// First are the day's earliest activities of the product, up to ?first=.
	First []DayItem `json:"first"`
}

// DayItem is one activity in a day summary.
type DayItem struct {
	ID         string `json:"id"`
	OccurredAt int64  `json:"occurred_at"`
	Action     string `json:"action"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

// Vocabularies answers GET /vocabularies.
type Vocabularies struct {
	Actions         []string       `json:"actions"`
	ActionPhrases   []ActionPhrase `json:"action_phrases"`
	Sources         []string       `json:"sources"`
	ImportKinds     []string       `json:"import_kinds"`
	RevisionReasons []string       `json:"revision_reasons"`
}

// ActionPhrase maps the export's words for a verb to an action.
type ActionPhrase struct {
	Phrase string `json:"phrase"`
	Action string `json:"action"`
}
