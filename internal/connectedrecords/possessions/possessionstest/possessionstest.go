// Package possessionstest holds the fixtures the possessions packages' tests
// share: a store over a fresh test database, and fakes of the photo source,
// the reference checker, the originals and the model caller. Every record it
// makes belongs to TestOwner, a made-up principal.
package possessionstest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

// TestOwner is the made-up principal every test's records belong to.
const TestOwner = "principal_000001"

// NewTestRoot is a store over a fresh test database, narrowed to nobody.
func NewTestRoot(t *testing.T, references possessions.ReferenceChecker) (*possessions.Store, *MemoryOriginals) {
	t.Helper()
	module, err := possessions.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	originals := NewMemoryOriginals()
	root, err := possessions.New(func(ctx context.Context, settings map[string]string) (*sql.DB, error) {
		return postgresstorage.OpenWithSessionSettings(ctx, database.URL(), module, settings)
	}, references, originals)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, originals
}

// NewTestStore is a store over a fresh test database, narrowed to TestOwner.
func NewTestStore(t *testing.T, references possessions.ReferenceChecker) *possessions.Store {
	t.Helper()
	root, _ := NewTestRoot(t, references)
	store, err := root.NarrowedToOwner(context.Background(), TestOwner)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// AsCaller serves handler with caller in the request context, as the bridge
// does after it verified the request's credential.
func AsCaller(caller ownership.Caller, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(ownership.ContextWithCaller(r.Context(), caller)))
	})
}

// AsTestOwner serves handler as TestOwner.
func AsTestOwner(handler http.Handler) http.Handler {
	return AsCaller(ownership.Caller{PrincipalID: TestOwner}, handler)
}

// MemoryOriginals keeps originals' bytes in memory.
type MemoryOriginals struct {
	mutex sync.Mutex
	files map[string][]byte
}

func NewMemoryOriginals() *MemoryOriginals { return &MemoryOriginals{files: map[string][]byte{}} }

func (originals *MemoryOriginals) Keep(_ context.Context, owner, sum, _ string, content []byte) (string, error) {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	id := fmt.Sprintf("file_%06d", len(originals.files)+1)
	originals.files[id] = append([]byte(nil), content...)
	return id, nil
}

func (originals *MemoryOriginals) KeepCopyOf(ctx context.Context, owner, sum, mediaType, path string) (string, error) {
	return "", fmt.Errorf("MemoryOriginals carries no files")
}

func (originals *MemoryOriginals) Open(_ context.Context, fileID string) (io.ReadCloser, error) {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	content, kept := originals.files[fileID]
	if !kept {
		return nil, fmt.Errorf("no file %s", fileID)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (originals *MemoryOriginals) Destroy(_ context.Context, fileID string) error {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	delete(originals.files, fileID)
	return nil
}

// FakeReferences knows a fixed set of other stores' ids.
type FakeReferences struct{ Known map[string]bool }

func (references FakeReferences) Check(ctx context.Context, ownerPrincipalID string, kind possessions.ReferenceKind, id string) error {
	if references.Known[id] {
		return nil
	}
	return fmt.Errorf("%w: no %s %s", possessions.ErrInvalid, kind, id)
}

// OpenTestStore is a store narrowed to TestOwner that knows two photos, a
// harness machine and a place.
func OpenTestStore(t *testing.T) *possessions.Store {
	t.Helper()
	return NewTestStore(t, FakeReferences{Known: map[string]bool{
		"photo_000001": true, "photo_000002": true, "m_test_machine": true, "place_000001": true,
	}})
}

// Fields decodes a JSON object of possession fields.
func Fields(t *testing.T, text string) map[string]json.RawMessage {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// ScriptedModels answers each reader by role and round from fixed readings.
type ScriptedModels struct {
	mutex   sync.Mutex
	Answers map[string]any // "<role> 1", "<role> 2", "<role> interpret"
	// Respond, when set, answers first: by role, step, the prompt and how
	// many pictures came with it; false falls through to answers.
	Respond func(role, step, prompt string, pictures int) (any, bool)
	Prompts []string
	Calls   map[string]int // "<role> <step>" → how many times asked
}

func (models *ScriptedModels) Ask(ctx context.Context, role, systemPrompt, prompt string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) (possessions.ModelAnswer, error) {
	step := "1"
	switch {
	case strings.Contains(prompt, "This photo will be cut to show the owner's outfit"):
		step = "wearer_box"
	case strings.Contains(prompt, "Reader A, made by another company, listed the looks"):
		step = "check_looks"
	case strings.Contains(prompt, "List the looks:"):
		step = "looks"
	case strings.Contains(prompt, "Sort these pictures into the individual physical pieces"):
		step = "pieces"
	case strings.Contains(prompt, "inventory says show this same piece"):
		step = "links"
	case strings.Contains(prompt, "one physical piece recorded twice"):
		step = "duplicates"
	case strings.Contains(prompt, "may be one outfit recorded twice"):
		step = "outfits"
	case strings.Contains(prompt, "a piece the owner has on in "):
		step = "match"
	case strings.Contains(prompt, "Which choice do they mean?"):
		step = "choice"
	case strings.Contains(prompt, "round two"):
		step = "2"
	case strings.Contains(prompt, "answered in their own words"):
		step = "interpret"
	case strings.Contains(prompt, "Write the item as it should now read"):
		step = "detail"
	case strings.Contains(prompt, "have no kind yet"):
		step = "classify"
	case strings.Contains(prompt, "Say for each whether it is about identity"):
		step = "sort"
	}
	models.mutex.Lock()
	models.Prompts = append(models.Prompts, prompt)
	if models.Calls == nil {
		models.Calls = map[string]int{}
	}
	models.Calls[role+" "+step]++
	models.mutex.Unlock()
	if models.Respond != nil {
		if answer, handled := models.Respond(role, step, prompt, len(images)); handled {
			encoded, _ := json.Marshal(answer)
			return possessions.ModelAnswer{Model: role + "-model", Parsed: encoded, InputTokens: 1000, OutputTokens: 100, CostUSD: 0.01}, nil
		}
	}
	answer, known := models.Answers[role+" "+step]
	if !known {
		return possessions.ModelAnswer{}, fmt.Errorf("no scripted answer for %s %s", role, step)
	}
	encoded, _ := json.Marshal(answer)
	return possessions.ModelAnswer{Model: role + "-model", Parsed: encoded, InputTokens: 1000, OutputTokens: 100, CostUSD: 0.01}, nil
}

// FakePhotos answers every photo with JPEG and a fixed wearer face.
type FakePhotos struct{}

// JPEG is a small picture with a gradient.
func JPEG() []byte {
	picture := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 40; x++ {
		for y := 0; y < 20; y++ {
			picture.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 12), 0, 255})
		}
	}
	var out bytes.Buffer
	_ = jpeg.Encode(&out, picture, nil)
	return out.Bytes()
}

func (FakePhotos) ReviewImage(ctx context.Context, photoID string) ([]byte, string, error) {
	return JPEG(), "thumbnail", nil
}

func (FakePhotos) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	return JPEG(), nil
}

func (FakePhotos) WearerFace(ctx context.Context, photoID, personID string) (possessions.Box, error) {
	return possessions.Box{Left: 0.4, Top: 0.1, Right: 0.6, Bottom: 0.3}, nil
}

func (FakePhotos) DetailImage(ctx context.Context, photoID string) (image.Image, string, error) {
	decoded, _, err := image.Decode(bytes.NewReader(JPEG()))
	return decoded, "thumbnail", err
}

func (FakePhotos) WhyACopy(ctx context.Context, photoID string) (string, error) {
	if photoID == "photo_000039" {
		return possessions.WhyACopy(`C:\\photos\\Screenshot_20260913_093002_Gallery.jpg`, nil), nil
	}
	return "", nil
}

func (FakePhotos) TakenAt(ctx context.Context, photoID string) (int64, error) {
	return 1794010000, nil // 2026-11-07 00:06 UTC, still 2026-11-06 in Los Angeles
}

// MustLocation is the time zone name names.
func MustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

// TestDay is the day DatedPhotos gives a photo it has no day for.
const TestDay = int64(1794010000) // 2026-11-06 in Los Angeles

// DatedPhotos is FakePhotos with a day of its own for each photo.
type DatedPhotos struct {
	FakePhotos
	PhotoTakenAt map[string]int64
}

func (photos DatedPhotos) TakenAt(ctx context.Context, photoID string) (int64, error) {
	if taken, known := photos.PhotoTakenAt[photoID]; known {
		return taken, nil
	}
	return TestDay, nil
}

// OpenAuditStore is a store narrowed to TestOwner that knows forty photos.
func OpenAuditStore(t *testing.T) *possessions.Store {
	t.Helper()
	known := map[string]bool{}
	for number := 1; number <= 40; number++ {
		known[fmt.Sprintf("photo_%06d", number)] = true
	}
	return NewTestStore(t, FakeReferences{Known: known})
}

// ClusterAnswers clusters, in a duplicate check, the labels of the given
// possession ids, per role.
func ClusterAnswers(clusters map[string][][]string, maximumPictures *int) func(role, step, prompt string, pictures int) (any, bool) {
	line := regexp.MustCompile(`(I\d+) \(picture \d+\) — (possession_\d+):`)
	var mutex sync.Mutex
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "duplicates" {
			return nil, false
		}
		mutex.Lock()
		if maximumPictures != nil && pictures > *maximumPictures {
			*maximumPictures = pictures
		}
		mutex.Unlock()
		labels := map[string]string{}
		for _, match := range line.FindAllStringSubmatch(prompt, -1) {
			labels[match[2]] = match[1]
		}
		answer := []map[string]any{}
		for _, cluster := range clusters[role] {
			members := []string{}
			for _, id := range cluster {
				if label, shown := labels[id]; shown {
					members = append(members, label)
				}
			}
			if len(members) >= 2 {
				answer = append(answer, map[string]any{"members": members, "reason": "same patch"})
			}
		}
		return map[string]any{"clusters": answer}, true
	}
}

// OpenProductTestStore is a store narrowed to TestOwner that knows a photo,
// an article and a transaction.
func OpenProductTestStore(t *testing.T) *possessions.Store {
	t.Helper()
	return NewTestStore(t, FakeReferences{Known: map[string]bool{"photo_000001": true, "article_000007": true, "transaction_000001": true}})
}

// AsymmetricPicture has a bright block in its top left corner and a ramp,
// so every turn of it looks different.
func AsymmetricPicture(width, height int) *image.RGBA {
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			level := uint8(40 + 100*x/width)
			if x < width/3 && y < height/4 {
				level = 250
			}
			picture.Set(x, y, color.RGBA{level, level, level, 255})
		}
	}
	return picture
}

// Kept is how many files the originals hold.
func (originals *MemoryOriginals) Kept() int {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	return len(originals.files)
}
