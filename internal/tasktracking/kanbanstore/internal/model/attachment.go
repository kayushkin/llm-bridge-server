package model

import (
	"encoding/json"
	"time"
)

// EventAttachmentAdded and EventAttachmentRemoved are a file being put on a
// card or taken off it. Like an assignment they have no kind-level clock
// state: a file says nothing about whether the work is runnable, so the event
// carries the state the card was already in.
const (
	EventAttachmentAdded   EventKind = "attachment_added"
	EventAttachmentRemoved EventKind = "attachment_removed"
)

// CardAttachment is a file on a card. This store owns the fact that the file
// hangs on the card, who may see it, and who it is for; file-store owns the
// file — its name, size, type and bytes — and File is its record passed
// through unchanged.
type CardAttachment struct {
	CardID string `json:"card_id"`
	// FileID is file-store's id (file_000001).
	FileID string `json:"file_id"`
	// Visibility is who the file is for, exactly as a note's is: `internal`
	// for the people working the card, `requester` for the person who asked as
	// well. Left out on upload it is `internal`.
	Visibility NoteVisibility `json:"visibility"`
	AttachedBy string         `json:"attached_by,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	// File is null when file-store no longer has the file — deleted there out
	// from under this row. The row is kept, so the gap can be seen.
	File json.RawMessage `json:"file" tstype:"FileStoreFile | null"`
}

// AttachmentEventDetail is the detail of both attachment events: enough to
// read the timeline without asking file-store, and the visibility, so a
// requester-facing reader of the timeline can drop the internal ones.
type AttachmentEventDetail struct {
	FileID      string         `json:"file_id"`
	Filename    string         `json:"filename"`
	SizeBytes   int64          `json:"size_bytes"`
	ContentType string         `json:"content_type"`
	Visibility  NoteVisibility `json:"visibility"`
}
