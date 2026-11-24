package agentstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	promptCollectionScopeGlobal  = "global"
	promptCollectionScopeProject = "project"
	// promptCollectionScopeContext is a collection that renders to no file:
	// its sections reach a session only when the session's context tags
	// select them. Its root is "" for every directory, or the directory it
	// is limited to.
	promptCollectionScopeContext = "context"

	PromptRevisionSourceImport = "import"
	PromptRevisionSourceUI     = "ui"
	PromptRevisionSourceDrift  = "drift"

	VersionSourcePromptRender = "prompt-render"

	promptSectionPositionStep = 100
)

// harnessConfigDirectoryNames are the per-repo folders a harness keeps its own
// files in. A prompt file inside one belongs to the repo that holds the folder,
// not to the folder.
var harnessConfigDirectoryNames = map[string]bool{
	".openclaw": true,
	".github":   true,
	".cursor":   true,
}

// isPromptProseFileName reports whether a tracked file name holds prompt prose
// and so can be an output of a collection: agentFileNames without the ones
// that are configuration in another syntax.
func isPromptProseFileName(base string) bool {
	return agentFileNames[base] && base != ".aider.conf.yml"
}

// ErrContextCollectionHasNoOutputs refuses a file output on a context
// collection: its sections reach a session only by tag, and a rendered file
// would hand them to every harness that reads it.
var ErrContextCollectionHasNoOutputs = errors.New("a context collection renders to no file: its sections reach a session only through its card's tags")

type PromptCollection struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Scope       string `json:"scope"`
	RootPath    string `json:"root_path"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	// RetiredAt is when RetirePromptCollection retired it; 0 while it is in use.
	RetiredAt int64 `json:"retired_at,omitempty"`
}

// PromptSection is one piece of the prompt. It names no harness and no file:
// every harness receives every enabled section of the collections that apply
// to its working directory.
//
// A section is described by Level and Title. Level 1 is a group, level 2 a
// section inside the group above it, level 0 text with no heading of its own
// (only sensible at the very top of a collection). Heading is the markdown
// line those two compose ("## Title") and is never written by a caller: two
// fields that could each say what a section is called disagreed the first time
// anyone edited one of them. A level-1 section with no body is an ordinary
// thing — a group that only holds sections — and is not an empty section.
type PromptSection struct {
	ID           int64    `json:"id"`
	CollectionID int64    `json:"collection_id"`
	Level        int      `json:"level"`
	Title        string   `json:"title"`
	Heading      string   `json:"heading"`
	Body         string   `json:"body"`
	Tags         []string `json:"tags"`
	Position     int      `json:"position"`
	Enabled      bool     `json:"enabled"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

type PromptSectionRevision struct {
	ID           int64    `json:"id"`
	SectionID    int64    `json:"section_id"`
	CollectionID int64    `json:"collection_id"`
	Operation    string   `json:"operation"`
	Title        string   `json:"title"`
	Heading      string   `json:"heading"`
	Body         string   `json:"body"`
	Tags         []string `json:"tags"`
	Position     int      `json:"position"`
	Enabled      bool     `json:"enabled"`
	Source       string   `json:"source"`
	DriftID      int64    `json:"drift_id,omitempty"`
	Note         string   `json:"note,omitempty"`
	CreatedAt    int64    `json:"created_at"`
}

// PromptCollectionOutput is a file a collection renders to, with how the file
// on disk compares to the render right now.
type PromptCollectionOutput struct {
	ID              int64  `json:"id"`
	CollectionID    int64  `json:"collection_id"`
	RelativePath    string `json:"relative_path"`
	Path            string `json:"path"`
	Enabled         bool   `json:"enabled"`
	AccountedSHA256 string `json:"accounted_sha256,omitempty"`
	AccountedAt     int64  `json:"accounted_at,omitempty"`

	// Live state, computed on read.
	ExistsOnDisk  bool   `json:"exists_on_disk"`
	DiskSHA256    string `json:"disk_sha256,omitempty"`
	Drifted       bool   `json:"drifted"`        // disk holds content the sections do not account for: neither the accounted content nor the current render
	MatchesRender bool   `json:"matches_render"` // disk is exactly the current render
	TrackedFileID int64  `json:"tracked_file_id,omitempty"`
}

type PromptCollectionView struct {
	Collection PromptCollection         `json:"collection"`
	Sections   []PromptSection          `json:"sections"`
	Outputs    []PromptCollectionOutput `json:"outputs"`
	Rendered   string                   `json:"rendered"`
	// OpenDrifts holds one open or held drift per edit; a drift's twins in
	// the collection's other files ride on it rather than appear again.
	OpenDrifts []PromptDrift `json:"open_drifts"`
}

// PromptRenderResult reports what a render wrote. RefusedReason is set, and
// nothing at all is written, when any output has drifted: writing the others
// would leave the collection's files disagreeing with each other.
type PromptRenderResult struct {
	View          *PromptCollectionView `json:"view"`
	WrittenFiles  []TrackedFile         `json:"written_files"`
	RefusedReason string                `json:"refused_reason,omitempty"`
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encodeTags(tags []string) string {
	clean := normalizeTags(tags)
	data, _ := json.Marshal(clean)
	return string(data)
}

func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	clean := []string{}
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		clean = append(clean, tag)
	}
	sort.Strings(clean)
	return clean
}

func decodeTags(raw string) []string {
	tags := []string{}
	if raw == "" {
		return tags
	}
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	return tags
}

// ---------------------------------------------------------------- collections

func (s *Store) getPromptCollection(id int64) (*PromptCollection, error) {
	var c PromptCollection
	err := s.db.QueryRow(`
		SELECT id, slug, title, scope, root_path, COALESCE(description,''), created_at, updated_at, retired_at
		FROM prompt_collections WHERE id=?`, id).
		Scan(&c.ID, &c.Slug, &c.Title, &c.Scope, &c.RootPath, &c.Description, &c.CreatedAt, &c.UpdatedAt, &c.RetiredAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListPromptCollections lists the collections in use. A retired collection is
// left out; getPromptCollection still finds it by id.
func (s *Store) ListPromptCollections() ([]PromptCollection, error) {
	rows, err := s.db.Query(`
		SELECT id, slug, title, scope, root_path, COALESCE(description,''), created_at, updated_at
		FROM prompt_collections WHERE retired_at=0
		ORDER BY CASE scope WHEN 'global' THEN 0 ELSE 1 END, root_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptCollection
	for rows.Next() {
		var c PromptCollection
		if err := rows.Scan(&c.ID, &c.Slug, &c.Title, &c.Scope, &c.RootPath, &c.Description, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListPromptCollectionViews() ([]PromptCollectionView, error) {
	collections, err := s.ListPromptCollections()
	if err != nil {
		return nil, err
	}
	views := make([]PromptCollectionView, 0, len(collections))
	for _, c := range collections {
		view, err := s.GetPromptCollectionView(c.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, *view)
	}
	return views, nil
}

func (s *Store) GetPromptCollectionView(collectionID int64) (*PromptCollectionView, error) {
	c, err := s.getPromptCollection(collectionID)
	if err != nil {
		return nil, err
	}
	sections, err := s.ListPromptSections(collectionID)
	if err != nil {
		return nil, err
	}
	rendered := renderPromptSections(sections)
	outputs, err := s.listPromptCollectionOutputs(*c, rendered)
	if err != nil {
		return nil, err
	}
	drifts, err := s.ListPromptDrifts(collectionID, []string{PromptDriftStatusOpen, PromptDriftStatusHeld})
	if err != nil {
		return nil, err
	}
	editsWaiting := []PromptDrift{}
	for _, drift := range drifts {
		if drift.StandsForItsTwins() {
			editsWaiting = append(editsWaiting, drift)
		}
	}
	return &PromptCollectionView{Collection: *c, Sections: sections, Outputs: outputs, Rendered: rendered, OpenDrifts: editsWaiting}, nil
}

// EnsurePromptCollection returns the collection for (scope, root), creating it
// when there is none.
func (s *Store) EnsurePromptCollection(scope, rootPath, title, description string) (*PromptCollection, error) {
	switch scope {
	case promptCollectionScopeGlobal, promptCollectionScopeProject:
		if rootPath == "" || !filepath.IsAbs(rootPath) {
			return nil, fmt.Errorf("a %s collection needs an absolute root_path", scope)
		}
		rootPath = filepath.Clean(rootPath)
	case promptCollectionScopeContext:
		if rootPath != "" && !filepath.IsAbs(rootPath) {
			return nil, fmt.Errorf("a context collection's root_path is empty, for every directory, or absolute")
		}
		if rootPath != "" {
			rootPath = filepath.Clean(rootPath)
		}
	default:
		return nil, fmt.Errorf("scope must be %q, %q or %q", promptCollectionScopeGlobal, promptCollectionScopeProject, promptCollectionScopeContext)
	}
	var id, retiredAt int64
	err := s.db.QueryRow(`SELECT id, retired_at FROM prompt_collections WHERE scope=? AND root_path=?`, scope, rootPath).Scan(&id, &retiredAt)
	if err == nil && retiredAt != 0 {
		return nil, fmt.Errorf("%w: collection %d, the %s collection rooted at %s, was retired", ErrPromptCollectionRetired, id, scope, rootPath)
	}
	if err == nil {
		return s.getPromptCollection(id)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	if title == "" && rootPath == "" {
		title = "Context for every directory"
	}
	if title == "" {
		title = filepath.Base(rootPath)
	}
	slug, err := s.unusedPromptCollectionSlug(scope, rootPath)
	if err != nil {
		return nil, err
	}
	ts := now()
	res, err := s.db.Exec(`
		INSERT INTO prompt_collections (slug, title, scope, root_path, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, slug, title, scope, rootPath, description, ts, ts)
	if err != nil {
		return nil, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.getPromptCollection(id)
}

func (s *Store) unusedPromptCollectionSlug(scope, rootPath string) (string, error) {
	replacer := strings.NewReplacer(" ", "-", "_", "-", ".", "-")
	var base string
	switch {
	case scope == promptCollectionScopeGlobal:
		base = "main"
	case scope == promptCollectionScopeContext && rootPath == "":
		base = "context"
	default:
		base = scope + "-" + strings.Trim(replacer.Replace(strings.ToLower(filepath.Base(rootPath))), "-")
	}
	slug := base
	for attempt := 2; ; attempt++ {
		var taken int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM prompt_collections WHERE slug=?`, slug).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, attempt)
	}
}

// ErrPromptCollectionMoveConflict is returned when moving a collection's root
// would collide with a row that already exists, or would strand an edit that
// is waiting on the old path. The HTTP layer answers it with 409.
var ErrPromptCollectionMoveConflict = errors.New("prompt collection move conflict")

// ErrPromptCollectionMoveInvalid is returned when the move asked for is not
// one a collection can make. The HTTP layer answers it with 400.
var ErrPromptCollectionMoveInvalid = errors.New("invalid prompt collection move")

// PromptCollectionRootMove reports what MovePromptCollectionRoot changed.
type PromptCollectionRootMove struct {
	CollectionID        int64                 `json:"collection_id"`
	PreviousRootPath    string                `json:"previous_root_path"`
	PreviousSlug        string                `json:"previous_slug"`
	RenamedTrackedFiles []TrackedFileRename   `json:"renamed_tracked_files"`
	MergedTrackedFiles  []TrackedFileMerge    `json:"merged_tracked_files"`
	View                *PromptCollectionView `json:"view"`
}

// TrackedFileMerge is a tracked_files row a scan created at a new path after
// the directory moved and before the collection did. Its versions moved onto
// the old row, which took over its path and file state, and the row is gone.
type TrackedFileMerge struct {
	MergedTrackedFileID int64  `json:"merged_tracked_file_id"`
	IntoTrackedFileID   int64  `json:"into_tracked_file_id"`
	Path                string `json:"path"`
	VersionsMoved       int64  `json:"versions_moved"`
}

// TrackedFileRename is one tracked_files row whose path a root move changed.
// The row keeps its id, so its version history stays with it.
type TrackedFileRename struct {
	TrackedFileID int64  `json:"tracked_file_id"`
	PreviousPath  string `json:"previous_path"`
	Path          string `json:"path"`
}

// MovePromptCollectionRoot points a project collection at a new root
// directory, for a repo whose directory was renamed. The collection keeps its
// id, sections, revisions and outputs; each tracked file at one of its output
// paths under the old root is renamed to the same relative path under the new
// root, so the file's version history stays attached. newSlug replaces the
// slug when not empty. Nothing on disk is touched.
//
// The scan runs on its own timer, so it may already have tracked a file at
// its new path when the move comes. Such a row belongs to no collection; it is
// merged into the old row (its versions move over, the old row takes its path
// and file state, and it is deleted) so one row carries the whole history.
//
// It refuses, and changes nothing, when the new root is not absolute, the
// collection is not a project collection, another collection already has the
// root or the slug, a target path is an output of another collection, or the
// collection has an open or held drift (that edit belongs to a file at the old
// path and must be settled first).
func (s *Store) MovePromptCollectionRoot(collectionID int64, newRootPath, newSlug string) (*PromptCollectionRootMove, error) {
	newRootPath = strings.TrimSpace(newRootPath)
	newSlug = strings.TrimSpace(newSlug)
	if newRootPath == "" || !filepath.IsAbs(newRootPath) {
		return nil, fmt.Errorf("%w: root_path %q must be an absolute path", ErrPromptCollectionMoveInvalid, newRootPath)
	}
	newRootPath = filepath.Clean(newRootPath)

	move := &PromptCollectionRootMove{CollectionID: collectionID, RenamedTrackedFiles: []TrackedFileRename{}, MergedTrackedFiles: []TrackedFileMerge{}}
	err := s.inPromptTransaction(func(tx *sql.Tx) error {
		var scope, oldRootPath, oldSlug string
		if err := tx.QueryRow(`SELECT scope, root_path, slug FROM prompt_collections WHERE id=?`, collectionID).
			Scan(&scope, &oldRootPath, &oldSlug); err != nil {
			return err
		}
		move.PreviousRootPath, move.PreviousSlug = oldRootPath, oldSlug
		if scope != promptCollectionScopeProject {
			return fmt.Errorf("%w: only a %q collection can move its root; collection %d is %q", ErrPromptCollectionMoveInvalid, promptCollectionScopeProject, collectionID, scope)
		}
		if newRootPath == oldRootPath && (newSlug == "" || newSlug == oldSlug) {
			return fmt.Errorf("%w: collection %d is already rooted at %s with that slug", ErrPromptCollectionMoveInvalid, collectionID, newRootPath)
		}

		var otherID int64
		err := tx.QueryRow(`SELECT id FROM prompt_collections WHERE scope=? AND root_path=? AND id<>?`, scope, newRootPath, collectionID).Scan(&otherID)
		if err == nil {
			return fmt.Errorf("%w: collection %d is already the %s collection rooted at %s", ErrPromptCollectionMoveConflict, otherID, scope, newRootPath)
		} else if err != sql.ErrNoRows {
			return err
		}
		if newSlug != "" {
			err := tx.QueryRow(`SELECT id FROM prompt_collections WHERE slug=? AND id<>?`, newSlug, collectionID).Scan(&otherID)
			if err == nil {
				return fmt.Errorf("%w: slug %q belongs to collection %d", ErrPromptCollectionMoveConflict, newSlug, otherID)
			} else if err != sql.ErrNoRows {
				return err
			}
		}

		var waitingDriftIDs []int64
		rows, err := tx.Query(`SELECT id FROM prompt_drifts WHERE collection_id=? AND status IN (?, ?) ORDER BY id`,
			collectionID, PromptDriftStatusOpen, PromptDriftStatusHeld)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			waitingDriftIDs = append(waitingDriftIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(waitingDriftIDs) > 0 {
			return fmt.Errorf("%w: collection %d has open or held drifts %v; apply or dismiss them before moving its root", ErrPromptCollectionMoveConflict, collectionID, waitingDriftIDs)
		}

		var relativePaths []string
		rows, err = tx.Query(`SELECT relative_path FROM prompt_collection_outputs WHERE collection_id=? ORDER BY id`, collectionID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var relativePath string
			if err := rows.Scan(&relativePath); err != nil {
				rows.Close()
				return err
			}
			relativePaths = append(relativePaths, relativePath)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		ts := now()
		if newRootPath != oldRootPath {
			for _, relativePath := range relativePaths {
				oldPath := filepath.Join(oldRootPath, relativePath)
				newPath := filepath.Join(newRootPath, relativePath)
				var claimingCollectionID int64
				err := tx.QueryRow(`
					SELECT o.collection_id FROM prompt_collection_outputs o
					JOIN prompt_collections c ON c.id = o.collection_id
					WHERE o.collection_id <> ? AND c.root_path || '/' || o.relative_path = ?`,
					collectionID, newPath).Scan(&claimingCollectionID)
				if err == nil {
					return fmt.Errorf("%w: %s is an output of collection %d", ErrPromptCollectionMoveConflict, newPath, claimingCollectionID)
				} else if err != sql.ErrNoRows {
					return err
				}
				var scannedAtTargetID int64
				err = tx.QueryRow(`SELECT id FROM tracked_files WHERE path=?`, newPath).Scan(&scannedAtTargetID)
				if err == sql.ErrNoRows {
					scannedAtTargetID = 0
				} else if err != nil {
					return err
				}
				var trackedFileID int64
				err = tx.QueryRow(`SELECT id FROM tracked_files WHERE path=?`, oldPath).Scan(&trackedFileID)
				if err == sql.ErrNoRows {
					continue // never scanned or rendered at the old path: the row at the new path, if any, is the whole history
				} else if err != nil {
					return err
				}
				if scannedAtTargetID != 0 {
					merge, err := mergeScannedTrackedFile(tx, scannedAtTargetID, trackedFileID, newPath, ts)
					if err != nil {
						return err
					}
					move.MergedTrackedFiles = append(move.MergedTrackedFiles, merge)
				}
				if _, err := tx.Exec(`UPDATE tracked_files SET path=?, updated_at=? WHERE id=?`, newPath, ts, trackedFileID); err != nil {
					return err
				}
				move.RenamedTrackedFiles = append(move.RenamedTrackedFiles, TrackedFileRename{TrackedFileID: trackedFileID, PreviousPath: oldPath, Path: newPath})
			}
		}

		slug := oldSlug
		if newSlug != "" {
			slug = newSlug
		}
		_, err = tx.Exec(`UPDATE prompt_collections SET root_path=?, slug=?, updated_at=? WHERE id=?`, newRootPath, slug, ts, collectionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	finalSlug := move.PreviousSlug
	if newSlug != "" {
		finalSlug = newSlug
	}
	log.Printf("prompt collection %d moved: root %s -> %s, slug %s -> %s, %d tracked files renamed %+v, %d scanned at the new path merged %+v",
		collectionID, move.PreviousRootPath, newRootPath, move.PreviousSlug, finalSlug, len(move.RenamedTrackedFiles), move.RenamedTrackedFiles, len(move.MergedTrackedFiles), move.MergedTrackedFiles)
	view, err := s.GetPromptCollectionView(collectionID)
	if err != nil {
		return nil, fmt.Errorf("collection %d moved, but reading it back failed: %w", collectionID, err)
	}
	move.View = view
	return move, nil
}

// mergeScannedTrackedFile folds the row a scan created at path into the row
// that carries the file's history: the versions and per-machine seed state
// move over (where the old row has no seed state for that machine), the old
// row takes the scanned row's file state, and the scanned row is deleted. The
// caller then renames the old row to path.
func mergeScannedTrackedFile(tx *sql.Tx, scannedID, intoID int64, path string, ts int64) (TrackedFileMerge, error) {
	merge := TrackedFileMerge{MergedTrackedFileID: scannedID, IntoTrackedFileID: intoID, Path: path}
	result, err := tx.Exec(`UPDATE tracked_file_versions SET tracked_file_id=? WHERE tracked_file_id=?`, intoID, scannedID)
	if err != nil {
		return merge, err
	}
	if merge.VersionsMoved, err = result.RowsAffected(); err != nil {
		return merge, err
	}
	if _, err := tx.Exec(`UPDATE OR IGNORE machine_seed_state SET tracked_file_id=? WHERE tracked_file_id=?`, intoID, scannedID); err != nil {
		return merge, err
	}
	if _, err := tx.Exec(`
		UPDATE tracked_files SET
			fs_hash = (SELECT fs_hash FROM tracked_files WHERE id=?),
			size = (SELECT size FROM tracked_files WHERE id=?),
			mtime = (SELECT mtime FROM tracked_files WHERE id=?),
			last_scanned_at = (SELECT last_scanned_at FROM tracked_files WHERE id=?),
			status = (SELECT status FROM tracked_files WHERE id=?),
			updated_at = ?
		WHERE id=?`, scannedID, scannedID, scannedID, scannedID, scannedID, ts, intoID); err != nil {
		return merge, err
	}
	// ON DELETE CASCADE takes any seed state the old row already had for that machine.
	if _, err := tx.Exec(`DELETE FROM tracked_files WHERE id=?`, scannedID); err != nil {
		return merge, err
	}
	return merge, nil
}

// ------------------------------------------------------------------- sections

func (s *Store) ListPromptSections(collectionID int64) ([]PromptSection, error) {
	rows, err := s.db.Query(`
		SELECT id, collection_id, title, heading, body, tags, position, enabled, created_at, updated_at
		FROM prompt_sections WHERE collection_id=? ORDER BY position, id`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptSection{}
	for rows.Next() {
		section, err := scanPromptSection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *section)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanPromptSection(row rowScanner) (*PromptSection, error) {
	var section PromptSection
	var tags string
	var enabled int
	if err := row.Scan(&section.ID, &section.CollectionID, &section.Title, &section.Heading, &section.Body, &tags, &section.Position, &enabled, &section.CreatedAt, &section.UpdatedAt); err != nil {
		return nil, err
	}
	section.Tags = decodeTags(tags)
	section.Enabled = enabled == 1
	section.Level = promptSectionHeadingLevel(section.Heading)
	return &section, nil
}

func (s *Store) GetPromptSection(id int64) (*PromptSection, error) {
	return scanPromptSection(s.db.QueryRow(`
		SELECT id, collection_id, title, heading, body, tags, position, enabled, created_at, updated_at
		FROM prompt_sections WHERE id=?`, id))
}

func renderPromptSections(sections []PromptSection) string {
	markdown := make([]PromptMarkdownSection, 0, len(sections))
	for _, section := range sections {
		if section.Enabled {
			markdown = append(markdown, PromptMarkdownSection{Heading: section.Heading, Body: section.Body})
		}
	}
	return RenderPromptMarkdown(markdown)
}

// promptSectionHeadingLevel is the number of leading '#' of a heading line, 0
// for no heading.
func promptSectionHeadingLevel(heading string) int {
	level := 0
	for level < len(heading) && heading[level] == '#' {
		level++
	}
	return level
}

// ComposePromptSectionHeading is the one place a heading line is built.
func ComposePromptSectionHeading(level int, title string) string {
	if level == 0 {
		return ""
	}
	return strings.Repeat("#", level) + " " + title
}

// validatePromptSection settles a section's Level, Title and Heading so the
// three always agree, and refuses one that would not read back from a rendered
// file as exactly itself. A caller holding a heading line cut from a file
// (import, drift) sets Heading; every other caller sets Level and Title.
func validatePromptSection(section *PromptSection) error {
	section.Body = trimTrailingSpaceLines(strings.Trim(strings.ReplaceAll(section.Body, "\r\n", "\n"), "\n"))
	section.Title = strings.TrimSpace(section.Title)
	section.Heading = strings.TrimSpace(section.Heading)
	if section.Heading != "" {
		if !isPromptSectionHeading(section.Heading) || strings.Contains(section.Heading, "\n") {
			return fmt.Errorf("heading %q is not one level-1 or level-2 markdown heading line", section.Heading)
		}
		section.Level = promptSectionHeadingLevel(section.Heading)
		section.Title = PromptSectionTitleFromHeading(section.Heading)
	}
	if section.Level < 0 || section.Level > promptSectionDeepestHeadingLevel {
		return fmt.Errorf("level must be 0 (no heading) to %d, got %d", promptSectionDeepestHeadingLevel, section.Level)
	}
	if strings.ContainsAny(section.Title, "\r\n") {
		return errors.New("title must be one line")
	}
	if section.Level > 0 {
		if section.Title == "" {
			return errors.New("a section with a heading needs a title")
		}
		if strings.HasPrefix(section.Title, "#") {
			return fmt.Errorf("title %q starts with '#': give the level as level, not in the title", section.Title)
		}
	} else {
		if section.Body == "" {
			return errors.New("a section with no heading needs a body")
		}
		if section.Title == "" {
			section.Title = PromptSectionTitleFromHeading("")
		}
	}
	section.Heading = ComposePromptSectionHeading(section.Level, section.Title)
	// A body that itself contains a section-level heading would split into two
	// sections the next time the rendered file is read back, and the drift
	// check would then see a change nobody made. The same check catches a
	// title that does not survive the trip.
	reparsed := SplitPromptMarkdown(RenderPromptMarkdown([]PromptMarkdownSection{{Heading: section.Heading, Body: section.Body}}))
	if len(reparsed) != 1 {
		return fmt.Errorf("body contains a level-1 or level-2 heading outside a code fence; it would read back as %d sections — make it a separate section or use a deeper heading", len(reparsed))
	}
	if reparsed[0].Heading != section.Heading || reparsed[0].Body != section.Body {
		return fmt.Errorf("section would not read back from a rendered file as written (heading %q came back as %q)", section.Heading, reparsed[0].Heading)
	}
	return nil
}

// promptSectionWriter is the part of *sql.DB and *sql.Tx the section writes use.
type promptSectionWriter interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

type promptRevisionContext struct {
	Source  string
	DriftID int64
	Note    string
}

func writePromptSectionRevision(w promptSectionWriter, section *PromptSection, operation string, ctx promptRevisionContext) error {
	var drift any
	if ctx.DriftID != 0 {
		drift = ctx.DriftID
	}
	_, err := w.Exec(`
		INSERT INTO prompt_section_revisions
			(section_id, collection_id, operation, title, heading, body, tags, position, enabled, source, drift_id, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		section.ID, section.CollectionID, operation, section.Title, section.Heading, section.Body,
		encodeTags(section.Tags), section.Position, boolToInt(section.Enabled), ctx.Source, drift, nullIfEmpty(ctx.Note), now())
	return err
}

func insertPromptSection(w promptSectionWriter, section *PromptSection, ctx promptRevisionContext) error {
	if err := validatePromptSection(section); err != nil {
		return err
	}
	ts := now()
	res, err := w.Exec(`
		INSERT INTO prompt_sections (collection_id, title, heading, body, tags, position, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		section.CollectionID, section.Title, section.Heading, section.Body, encodeTags(section.Tags), section.Position, boolToInt(section.Enabled), ts, ts)
	if err != nil {
		return err
	}
	if section.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	section.Tags = normalizeTags(section.Tags)
	return writePromptSectionRevision(w, section, "create", ctx)
}

func updatePromptSection(w promptSectionWriter, section *PromptSection, ctx promptRevisionContext) error {
	if err := validatePromptSection(section); err != nil {
		return err
	}
	res, err := w.Exec(`
		UPDATE prompt_sections SET title=?, heading=?, body=?, tags=?, position=?, enabled=?, updated_at=? WHERE id=?`,
		section.Title, section.Heading, section.Body, encodeTags(section.Tags), section.Position, boolToInt(section.Enabled), now(), section.ID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	section.Tags = normalizeTags(section.Tags)
	return writePromptSectionRevision(w, section, "update", ctx)
}

func deletePromptSection(w promptSectionWriter, section *PromptSection, ctx promptRevisionContext) error {
	if err := writePromptSectionRevision(w, section, "delete", ctx); err != nil {
		return err
	}
	_, err := w.Exec(`DELETE FROM prompt_sections WHERE id=?`, section.ID)
	return err
}

func (s *Store) inPromptTransaction(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// CreatePromptSection adds a section and renders the collection. Placement,
// first rule that applies: afterSectionID names the section it follows (the
// collection is renumbered when the neighbors leave no room); else a non-zero
// section.Position is taken as given; else the section goes last.
func (s *Store) CreatePromptSection(collectionID int64, section *PromptSection, afterSectionID int64, note string) (*PromptRenderResult, error) {
	if _, err := s.getPromptCollection(collectionID); err != nil {
		return nil, err
	}
	section.CollectionID = collectionID
	err := s.inPromptTransaction(func(tx *sql.Tx) error {
		switch {
		case afterSectionID != 0:
			var owner int64
			if err := tx.QueryRow(`SELECT collection_id FROM prompt_sections WHERE id=?`, afterSectionID).Scan(&owner); err != nil || owner != collectionID {
				return fmt.Errorf("after_section_id %d is not a section of collection %d", afterSectionID, collectionID)
			}
			position, err := promptSectionPositionForInsert(tx, collectionID, PromptDriftOperation{AfterSectionID: afterSectionID})
			if err != nil {
				return err
			}
			section.Position = position
		case section.Position == 0:
			var last sql.NullInt64
			if err := tx.QueryRow(`SELECT MAX(position) FROM prompt_sections WHERE collection_id=?`, collectionID).Scan(&last); err != nil {
				return err
			}
			section.Position = int(last.Int64) + promptSectionPositionStep
		}
		return insertPromptSection(tx, section, promptRevisionContext{Source: PromptRevisionSourceUI, Note: note})
	})
	if err != nil {
		return nil, err
	}
	return s.RenderPromptCollection(collectionID)
}

func (s *Store) UpdatePromptSection(sectionID int64, section *PromptSection, note string) (*PromptRenderResult, error) {
	existing, err := s.GetPromptSection(sectionID)
	if err != nil {
		return nil, err
	}
	section.ID = sectionID
	section.CollectionID = existing.CollectionID
	if err := s.inPromptTransaction(func(tx *sql.Tx) error {
		return updatePromptSection(tx, section, promptRevisionContext{Source: PromptRevisionSourceUI, Note: note})
	}); err != nil {
		return nil, err
	}
	return s.RenderPromptCollection(existing.CollectionID)
}

func (s *Store) DeletePromptSection(sectionID int64, note string) (*PromptRenderResult, error) {
	existing, err := s.GetPromptSection(sectionID)
	if err != nil {
		return nil, err
	}
	if err := s.inPromptTransaction(func(tx *sql.Tx) error {
		return deletePromptSection(tx, existing, promptRevisionContext{Source: PromptRevisionSourceUI, Note: note})
	}); err != nil {
		return nil, err
	}
	return s.RenderPromptCollection(existing.CollectionID)
}

// ListPromptSectionRevisions returns history newest first. sectionID 0 means
// the whole collection, deleted sections included.
func (s *Store) ListPromptSectionRevisions(collectionID, sectionID int64, limit int) ([]PromptSectionRevision, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, section_id, collection_id, operation, title, heading, body, tags, position, enabled, source, COALESCE(drift_id,0), COALESCE(note,''), created_at
		FROM prompt_section_revisions WHERE collection_id=?`
	args := []any{collectionID}
	if sectionID != 0 {
		query += ` AND section_id=?`
		args = append(args, sectionID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptSectionRevision{}
	for rows.Next() {
		var revision PromptSectionRevision
		var tags string
		var enabled int
		if err := rows.Scan(&revision.ID, &revision.SectionID, &revision.CollectionID, &revision.Operation, &revision.Title, &revision.Heading, &revision.Body, &tags, &revision.Position, &enabled, &revision.Source, &revision.DriftID, &revision.Note, &revision.CreatedAt); err != nil {
			return nil, err
		}
		revision.Tags = decodeTags(tags)
		revision.Enabled = enabled == 1
		out = append(out, revision)
	}
	return out, rows.Err()
}

// -------------------------------------------------------------------- outputs

func validatePromptOutputRelativePath(relativePath string) (string, error) {
	relativePath = filepath.Clean(relativePath)
	if relativePath == "." || filepath.IsAbs(relativePath) || strings.HasPrefix(relativePath, "..") {
		return "", fmt.Errorf("relative_path %q must stay inside the collection root", relativePath)
	}
	if !isPromptProseFileName(filepath.Base(relativePath)) {
		return "", fmt.Errorf("%q is not a prompt file name the scan tracks", filepath.Base(relativePath))
	}
	return relativePath, nil
}

// AddPromptCollectionOutput registers a file the collection renders to. It
// writes nothing to disk; the next render does.
func (s *Store) AddPromptCollectionOutput(collectionID int64, relativePath string) (*PromptCollectionView, error) {
	relativePath, err := validatePromptOutputRelativePath(relativePath)
	if err != nil {
		return nil, err
	}
	collection, err := s.getPromptCollection(collectionID)
	if err != nil {
		return nil, err
	}
	if collection.Scope == promptCollectionScopeContext {
		return nil, ErrContextCollectionHasNoOutputs
	}
	ts := now()
	if _, err := s.db.Exec(`
		INSERT INTO prompt_collection_outputs (collection_id, relative_path, enabled, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?) ON CONFLICT (collection_id, relative_path) DO NOTHING`, collectionID, relativePath, ts, ts); err != nil {
		return nil, err
	}
	return s.GetPromptCollectionView(collectionID)
}

// SetPromptCollectionOutputEnabled turns rendering to one file on or off. A
// disabled output is left on disk exactly as it is.
func (s *Store) SetPromptCollectionOutputEnabled(outputID int64, enabled bool) (*PromptCollectionView, error) {
	var collectionID int64
	if err := s.db.QueryRow(`SELECT collection_id FROM prompt_collection_outputs WHERE id=?`, outputID).Scan(&collectionID); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE prompt_collection_outputs SET enabled=?, updated_at=? WHERE id=?`, boolToInt(enabled), now(), outputID); err != nil {
		return nil, err
	}
	return s.GetPromptCollectionView(collectionID)
}

func (s *Store) listPromptCollectionOutputs(c PromptCollection, rendered string) ([]PromptCollectionOutput, error) {
	rows, err := s.db.Query(`
		SELECT id, collection_id, relative_path, enabled, COALESCE(accounted_sha256,''), COALESCE(accounted_at,0)
		FROM prompt_collection_outputs WHERE collection_id=? ORDER BY id`, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	renderedSHA := sha256Hex([]byte(rendered))
	out := []PromptCollectionOutput{}
	for rows.Next() {
		var output PromptCollectionOutput
		var enabled int
		if err := rows.Scan(&output.ID, &output.CollectionID, &output.RelativePath, &enabled, &output.AccountedSHA256, &output.AccountedAt); err != nil {
			return nil, err
		}
		output.Enabled = enabled == 1
		output.Path = filepath.Join(c.RootPath, output.RelativePath)
		out = append(out, output)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		data, err := os.ReadFile(out[i].Path)
		switch {
		case err == nil:
			out[i].ExistsOnDisk = true
			out[i].DiskSHA256 = sha256Hex(data)
			out[i].MatchesRender = out[i].DiskSHA256 == renderedSHA
			out[i].Drifted = out[i].DiskSHA256 != out[i].AccountedSHA256 && !out[i].MatchesRender
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("read %s: %w", out[i].Path, err)
		}
		if file, err := s.findTrackedFileByPath(out[i].Path); err == nil {
			out[i].TrackedFileID = file.ID
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) findTrackedFileByPath(path string) (*TrackedFile, error) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM tracked_files WHERE path=?`, filepath.Clean(path)).Scan(&id); err != nil {
		return nil, err
	}
	return s.GetTrackedFile(id)
}

func (s *Store) markPromptOutputAccounted(w promptSectionWriter, outputID int64, sha string) error {
	_, err := w.Exec(`UPDATE prompt_collection_outputs SET accounted_sha256=?, accounted_at=?, updated_at=? WHERE id=?`, sha, now(), now(), outputID)
	return err
}

// RenderPromptCollection writes the collection's sections to every enabled
// output. It first looks for drift and, if it finds any it cannot settle,
// writes nothing: see PromptRenderResult. A drifted file whose drift the
// operator dismissed is overwritten — that is what dismissing means — and its
// content is already kept on the drift row and in the file's version history.
func (s *Store) RenderPromptCollection(collectionID int64) (*PromptRenderResult, error) {
	c, err := s.getPromptCollection(collectionID)
	if err != nil {
		return nil, err
	}
	if _, err := s.DetectPromptDrifts(collectionID); err != nil {
		return nil, fmt.Errorf("detect drift: %w", err)
	}
	view, err := s.GetPromptCollectionView(collectionID)
	if err != nil {
		return nil, err
	}
	result := &PromptRenderResult{View: view, WrittenFiles: []TrackedFile{}}

	var blocked []string
	for _, output := range view.Outputs {
		if !output.Enabled || !output.ExistsOnDisk || !output.Drifted {
			continue
		}
		dismissed, err := s.promptDriftDismissed(output.ID, output.DiskSHA256)
		if err != nil {
			return nil, err
		}
		if !dismissed {
			blocked = append(blocked, output.Path)
		}
	}
	if len(blocked) > 0 {
		result.RefusedReason = fmt.Sprintf("not rendered: %s changed on disk since the sections last accounted for it — apply or dismiss the drift first", strings.Join(blocked, ", "))
		return result, nil
	}
	if strings.TrimSpace(view.Rendered) == "" {
		result.RefusedReason = "not rendered: the collection has no enabled section, and an empty prompt file is never what was meant"
		return result, nil
	}

	content := []byte(view.Rendered)
	renderedSHA := sha256Hex(content)
	for _, output := range view.Outputs {
		if !output.Enabled {
			continue
		}
		if output.MatchesRender {
			if output.AccountedSHA256 != renderedSHA {
				if err := s.settleOutputTheSectionsAlreadyRender(output.ID, renderedSHA); err != nil {
					return nil, err
				}
			}
			continue
		}
		file, err := s.writePromptOutputFile(*c, output, content)
		if err != nil {
			return nil, fmt.Errorf("write %s: %w", output.Path, err)
		}
		if err := s.markPromptOutputAccounted(s.db, output.ID, renderedSHA); err != nil {
			return nil, err
		}
		result.WrittenFiles = append(result.WrittenFiles, *file)
	}
	if _, err := s.db.Exec(`UPDATE prompt_collections SET updated_at=? WHERE id=?`, now(), collectionID); err != nil {
		return nil, err
	}
	if result.View, err = s.GetPromptCollectionView(collectionID); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) writePromptOutputFile(c PromptCollection, output PromptCollectionOutput, content []byte) (*TrackedFile, error) {
	if err := os.MkdirAll(filepath.Dir(output.Path), 0755); err != nil {
		return nil, err
	}
	file, err := s.findTrackedFileByPath(output.Path)
	if err == sql.ErrNoRows {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, homeErr
		}
		scope, ok := classifyFile(output.Path, home)
		if !ok {
			return nil, fmt.Errorf("%s is not a path the scan would track", output.Path)
		}
		// The row has to exist before the versioned write can attach history to it.
		id, _, upsertErr := s.upsertTrackedFile(output.Path, scope, "", true, "", 0, 0)
		if upsertErr != nil {
			return nil, upsertErr
		}
		file, err = s.GetTrackedFile(id)
	}
	if err != nil {
		return nil, err
	}
	if !file.Enabled {
		return nil, fmt.Errorf("%s is disabled (.disabled on disk); enable it or disable this output", output.Path)
	}
	written, _, err := s.WriteTrackedFileVersioned(file.ID, content, VersionSourcePromptRender, "", "rendered from prompt collection "+c.Slug)
	return written, err
}

// --------------------------------------------------------------------- import

// PromptImportFileReport says how faithfully one file came through the split.
type PromptImportFileReport struct {
	Path                 string `json:"path"`
	Sections             int    `json:"sections"`
	SectionsAlreadyKnown int    `json:"sections_already_known"`
	ByteIdentical        bool   `json:"byte_identical"` // render(split(file)) == file
}

type PromptImportReport struct {
	CollectionID int64                    `json:"collection_id"`
	Files        []PromptImportFileReport `json:"files"`
}

// ImportPromptCollectionFromDisk fills an empty collection from the files at
// relativePaths, in that order, and registers each as an output. A section
// whose heading and body already came in from an earlier file is not added
// twice. Each file is checked before anything is stored: splitting it and
// rendering the pieces must give back the same text, spacing aside. If any
// file fails that, nothing is written.
func (s *Store) ImportPromptCollectionFromDisk(collectionID int64, relativePaths []string) (*PromptImportReport, error) {
	c, err := s.getPromptCollection(collectionID)
	if err != nil {
		return nil, err
	}
	var existing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM prompt_sections WHERE collection_id=?`, collectionID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing > 0 {
		return nil, fmt.Errorf("collection %d already has %d sections; import only fills an empty collection", collectionID, existing)
	}

	type importedFile struct {
		relativePath string
		sha          string
		sections     []PromptMarkdownSection
	}
	report := &PromptImportReport{CollectionID: collectionID}
	var files []importedFile
	for _, relativePath := range relativePaths {
		relativePath, err := validatePromptOutputRelativePath(relativePath)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(c.RootPath, relativePath)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sections := SplitPromptMarkdown(string(data))
		rendered := RenderPromptMarkdown(sections)
		if collapseWhitespace(rendered) != collapseWhitespace(string(data)) {
			return nil, fmt.Errorf("%s: splitting and re-rendering changed the text; refusing to import", path)
		}
		files = append(files, importedFile{relativePath: relativePath, sha: sha256Hex(data), sections: sections})
		report.Files = append(report.Files, PromptImportFileReport{Path: path, Sections: len(sections), ByteIdentical: rendered == string(data)})
	}

	err = s.inPromptTransaction(func(tx *sql.Tx) error {
		known := map[PromptMarkdownSection]bool{}
		position := 0
		for fileIndex, file := range files {
			for _, markdown := range file.sections {
				if known[markdown] {
					report.Files[fileIndex].SectionsAlreadyKnown++
					continue
				}
				known[markdown] = true
				position += promptSectionPositionStep
				section := &PromptSection{CollectionID: collectionID, Heading: markdown.Heading, Body: markdown.Body, Position: position, Enabled: true, Tags: []string{}}
				if err := insertPromptSection(tx, section, promptRevisionContext{Source: PromptRevisionSourceImport, Note: "imported from " + file.relativePath}); err != nil {
					return fmt.Errorf("%s, section %q: %w", file.relativePath, markdown.Heading, err)
				}
			}
			ts := now()
			if _, err := tx.Exec(`
				INSERT INTO prompt_collection_outputs (collection_id, relative_path, enabled, accounted_sha256, accounted_at, created_at, updated_at)
				VALUES (?, ?, 1, ?, ?, ?, ?)
				ON CONFLICT (collection_id, relative_path) DO UPDATE SET accounted_sha256=excluded.accounted_sha256, accounted_at=excluded.accounted_at, updated_at=excluded.updated_at`,
				collectionID, file.relativePath, file.sha, ts, ts, ts); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// PromptCollectionRootForFile says which collection a tracked prompt file
// belongs to: the directory it sits in, or the repo when it sits in a
// harness's own folder inside the repo.
func PromptCollectionRootForFile(path string) (root, relativePath string) {
	// The outermost harness folder on the path decides: everything beneath
	// repo/.openclaw/, however deep, belongs to repo.
	outermost := ""
	for directory := filepath.Dir(path); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
		if harnessConfigDirectoryNames[filepath.Base(directory)] {
			outermost = directory
		}
	}
	if outermost == "" {
		return filepath.Dir(path), filepath.Base(path)
	}
	root = filepath.Dir(outermost)
	relativePath, _ = filepath.Rel(root, path)
	return root, relativePath
}

// ImportUntrackedPromptFiles creates a collection for every present,
// not-ignored global or project prompt file that no collection has as an
// output yet, and imports it. Files in one directory (or one repo's harness
// folders) share a collection. globalImportOrder names the order the global
// files are read in, by relative path; files not named follow in path order.
func (s *Store) ImportUntrackedPromptFiles(globalImportOrder []string) ([]PromptImportReport, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	files, err := s.ListTrackedFiles("", "")
	if err != nil {
		return nil, err
	}
	type group struct {
		scope         string
		relativePaths []string
	}
	retiredRoots, err := s.retiredPromptCollectionRoots()
	if err != nil {
		return nil, err
	}
	groups := map[string]*group{}
	var roots []string
	for _, file := range files {
		if file.Status != "present" || !file.Enabled || !isPromptProseFileName(filepath.Base(file.Path)) {
			continue
		}
		if file.Scope != "global" && file.Scope != "project" {
			continue
		}
		root, relativePath := PromptCollectionRootForFile(file.Path)
		scope := promptCollectionScopeProject
		if root == home {
			if file.Scope != "global" {
				// A harness's own home folder (~/.openclaw/AGENTS.md) is that
				// harness's state, not a prompt for this host.
				continue
			}
			scope = promptCollectionScopeGlobal
		}
		if retiredID, retired := retiredRoots[scope][root]; retired {
			// Someone retired this root's collection on purpose; importing it
			// again would undo that, and EnsurePromptCollection refuses it.
			log.Printf("agent-store: import leaves %s alone: collection %d for %s is retired", file.Path, retiredID, root)
			continue
		}
		var claimed int
		if err := s.db.QueryRow(`
			SELECT COUNT(*) FROM prompt_collection_outputs o JOIN prompt_collections c ON c.id=o.collection_id
			WHERE c.root_path=? AND o.relative_path=?`, root, relativePath).Scan(&claimed); err != nil {
			return nil, err
		}
		if claimed > 0 {
			continue
		}
		if groups[root] == nil {
			groups[root] = &group{scope: scope}
			roots = append(roots, root)
		}
		groups[root].relativePaths = append(groups[root].relativePaths, relativePath)
	}
	sort.Strings(roots)

	var reports []PromptImportReport
	for _, root := range roots {
		g := groups[root]
		sort.Strings(g.relativePaths)
		if g.scope == promptCollectionScopeGlobal {
			g.relativePaths = orderedFirst(g.relativePaths, globalImportOrder)
		}
		title, description := filepath.Base(root), "Prompt sections for work under "+root+"."
		if g.scope == promptCollectionScopeGlobal {
			title, description = "Host prompt", "Prompt sections every harness on this host receives."
		}
		c, err := s.EnsurePromptCollection(g.scope, root, title, description)
		if err != nil {
			return nil, err
		}
		var sectionCount int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM prompt_sections WHERE collection_id=?`, c.ID).Scan(&sectionCount); err != nil {
			return nil, err
		}
		if sectionCount > 0 {
			return reports, fmt.Errorf("%s has prompt files no collection renders to (%s) but collection %d already has sections; add them as outputs by hand after checking their content", root, strings.Join(g.relativePaths, ", "), c.ID)
		}
		report, err := s.ImportPromptCollectionFromDisk(c.ID, g.relativePaths)
		if err != nil {
			return reports, err
		}
		reports = append(reports, *report)
	}
	return reports, nil
}

func orderedFirst(values, first []string) []string {
	present := map[string]bool{}
	for _, value := range values {
		present[value] = true
	}
	var out []string
	placed := map[string]bool{}
	for _, value := range first {
		if present[value] && !placed[value] {
			out = append(out, value)
			placed[value] = true
		}
	}
	for _, value := range values {
		if !placed[value] {
			out = append(out, value)
		}
	}
	return out
}
