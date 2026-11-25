package photostore

// Wire types: what the routes take and answer. ./generate-ts.sh renders this
// file and vocabulary.go to ts/model.ts.

// Photo is one photo or video from the user's own files. Timestamps are epoch
// seconds; 0 means unset.
type Photo struct {
	// ID is photo_000001: what anything else joins on.
	ID string `json:"id"`
	// Sha256 is the original file's hash; an import finds the photo by it.
	Sha256 string `json:"sha256"`
	// SourceMachine and SourcePath say where the original is.
	SourceMachine string `json:"source_machine"`
	SourcePath    string `json:"source_path"`
	Folder        string `json:"folder"`
	MediaType     string `json:"media_type"`
	TakenAt       int64  `json:"taken_at"`
	// DateSource is one of GET /date-sources: how far to trust TakenAt.
	DateSource string   `json:"date_source"`
	Latitude   *float64 `json:"latitude"`
	Longitude  *float64 `json:"longitude"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	// Description is markdown; [text](ref:<id>) links name people and events.
	Description string   `json:"description"`
	DescribedBy string   `json:"described_by"`
	Tags        []string `json:"tags"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	DeletedAt   int64    `json:"deleted_at"`
	// PurgedAt is when the photo was purged: its thumbnail and description are
	// gone for good, it stays deleted, and an import never brings it back.
	// 0 = never.
	PurgedAt int64 `json:"purged_at"`
}

// ImportPhoto is one photo in POST /imports: its facts and its thumbnail.
type ImportPhoto struct {
	Sha256        string   `json:"sha256"`
	SourceMachine string   `json:"source_machine"`
	SourcePath    string   `json:"source_path"`
	Folder        string   `json:"folder"`
	MediaType     string   `json:"media_type"`
	TakenAt       int64    `json:"taken_at"`
	DateSource    string   `json:"date_source"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
	Width         int      `json:"width"`
	Height        int      `json:"height"`
	// ThumbnailJPEG is the thumbnail's bytes, base64 in JSON.
	ThumbnailJPEG []byte `json:"thumbnail_jpeg"`
}

// ImportRequest is the body of POST /imports.
type ImportRequest struct {
	Photos []ImportPhoto `json:"photos"`
}

// ImportResult says what POST /imports did.
type ImportResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	// FacesRetired counts the faces retired because an import replaced their
	// photo's thumbnail with a different picture: they were measured on the
	// old one, so the photo waits to be scanned again.
	FacesRetired int `json:"faces_retired"`
	// IDs is each photo's id, in request order.
	IDs []string `json:"ids"`
}

// PhotoPatch is the body of PATCH /photos/{id}. A nil field is left alone.
type PhotoPatch struct {
	Description *string   `json:"description,omitempty"`
	DescribedBy *string   `json:"described_by,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	TakenAt     *int64    `json:"taken_at,omitempty"`
	DateSource  *string   `json:"date_source,omitempty"`
}

// Revision is the whole state of a photo just before one change.
type Revision struct {
	RevisionID int64  `json:"revision_id"`
	Reason     string `json:"reason"`
	ReplacedAt int64  `json:"replaced_at"`
	Photo      Photo  `json:"photo"`
}

// TagCount is one row of GET /tags: a tag and how many live photos carry it.
type TagCount struct {
	Tag    string `json:"tag"`
	Photos int    `json:"photos"`
}

// Counts is what GET /health counts.
type Counts struct {
	Photos    int `json:"photos"`
	Described int `json:"described"`
	WithGPS   int `json:"with_gps"`
	Deleted   int `json:"deleted"`
	Revisions int `json:"revisions"`
}

// Health is the answer to GET /health.
type Health struct {
	Status string `json:"status"`
	Counts Counts `json:"counts"`
}

// TakenActivityBucket is one span of time in GET /taken-activity: how many
// photos were taken in it, and a few of them spread across it, so a page can
// draw a strip of thumbnails over years without listing every photo.
type TakenActivityBucket struct {
	// Start is the span's first instant, epoch seconds, in the time zone asked.
	Start int64 `json:"start"`
	// Photos counts the live photos taken in the span that the filter keeps.
	Photos int `json:"photos"`
	// SamplePhotoIDs are up to `samples` of them, evenly spaced in time order.
	SamplePhotoIDs []string `json:"sample_photo_ids"`
}
