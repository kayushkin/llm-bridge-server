// Package filestore owns files: bytes somebody uploaded, under a name, for a
// thing that lives in another service. It is the one place on this host a
// ticket's attachment, a mail's attachment or a chat's image is kept.
//
// It knows nothing about what a file is attached to and decides nothing about
// who may read it. A file is readable exactly when the service that owns the
// thing it hangs on says so, and that service is who calls this one: kanban-store
// checks the caller's access to the card and then fetches the bytes with the
// service token. So this service binds to loopback, takes no call without that
// token, and must never be proxied to a browser.
package filestore

// File is one upload. See schema.sql for what each field means and why the
// bytes are not part of it.
type File struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	// ContentType is what the uploader declared, without parameters.
	// DetectedContentType is what the first bytes look like. They differ for an
	// honest reason as often as a dishonest one — a .csv declared text/csv is
	// detected text/plain — so neither is refused; a reader that cares compares.
	ContentType         string `json:"content_type"`
	DetectedContentType string `json:"detected_content_type"`
	Filename            string `json:"filename"`
	// OwnerService and OwnerRef say who put the file here and for what of
	// theirs: "kanban-store" and a card id.
	OwnerService string `json:"owner_service"`
	OwnerRef     string `json:"owner_ref"`
	// UploadedByPrincipalID is a principal-store id, or empty. This store
	// checks its shape and not its existence: the calling service has already
	// authenticated that principal, and vouches for it.
	UploadedByPrincipalID string `json:"uploaded_by_principal_id,omitempty"`
	CreatedAt             int64  `json:"created_at"`
	// DeletedAt is 0 for a live file. DELETE is reversible and sets it.
	DeletedAt int64 `json:"deleted_at,omitempty"`
}

// Limits is GET /limits: what an upload form must not hardcode.
type Limits struct {
	MaximumFileBytes int64 `json:"maximum_file_bytes"`
	// InlineContentTypes are the declared types this store will serve for
	// display in a browser when asked to (?inline=true). Every other file is
	// served as a download, whatever was asked.
	InlineContentTypes []string `json:"inline_content_types"`
}

// Health is GET /health.
type Health struct {
	Status string `json:"status"`
	// Files counts live rows; Blobs the distinct byte strings they and the
	// deleted rows still name; BlobBytes what those take on disk.
	Files     int64 `json:"files"`
	Blobs     int64 `json:"blobs"`
	BlobBytes int64 `json:"blob_bytes"`
}
