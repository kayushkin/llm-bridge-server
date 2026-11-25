package possessionstore

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testOwner is the made-up principal every test's records belong to.
const testOwner = "principal_000001"

// newTestRoot is a store over a fresh test database, narrowed to nobody.
func newTestRoot(t *testing.T, references ReferenceChecker) (*Store, *memoryOriginals) {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	originals := newMemoryOriginals()
	root, err := New(func(ctx context.Context, settings map[string]string) (*sql.DB, error) {
		return postgresstorage.OpenWithSessionSettings(ctx, database.URL(), module, settings)
	}, references, originals)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, originals
}

// newTestStore is a store over a fresh test database, narrowed to testOwner.
func newTestStore(t *testing.T, references ReferenceChecker) *Store {
	t.Helper()
	root, _ := newTestRoot(t, references)
	store, err := root.NarrowedToOwner(context.Background(), testOwner)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// asCaller serves handler with caller in the request context, as the bridge
// does after it verified the request's credential.
func asCaller(caller ownership.Caller, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(ownership.ContextWithCaller(r.Context(), caller)))
	})
}

// asTestOwner serves handler as testOwner.
func asTestOwner(handler http.Handler) http.Handler {
	return asCaller(ownership.Caller{PrincipalID: testOwner}, handler)
}

// memoryOriginals keeps originals' bytes in memory.
type memoryOriginals struct {
	mutex sync.Mutex
	files map[string][]byte
}

func newMemoryOriginals() *memoryOriginals { return &memoryOriginals{files: map[string][]byte{}} }

func (originals *memoryOriginals) Keep(_ context.Context, owner, sum, _ string, content []byte) (string, error) {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	id := fmt.Sprintf("file_%06d", len(originals.files)+1)
	originals.files[id] = append([]byte(nil), content...)
	return id, nil
}

func (originals *memoryOriginals) KeepCopyOf(ctx context.Context, owner, sum, mediaType, path string) (string, error) {
	return "", fmt.Errorf("memoryOriginals carries no files")
}

func (originals *memoryOriginals) Open(_ context.Context, fileID string) (io.ReadCloser, error) {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	content, kept := originals.files[fileID]
	if !kept {
		return nil, fmt.Errorf("no file %s", fileID)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (originals *memoryOriginals) Destroy(_ context.Context, fileID string) error {
	originals.mutex.Lock()
	defer originals.mutex.Unlock()
	delete(originals.files, fileID)
	return nil
}
