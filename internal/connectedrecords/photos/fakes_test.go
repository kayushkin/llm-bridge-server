package photos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// keptThumbnail is one file the fake keeps.
type keptThumbnail struct {
	ownerPrincipalID string
	photoID          string
	content          []byte
}

// fakeThumbnailFiles stands in for file-store: files in memory, by id.
type fakeThumbnailFiles struct {
	mutex   sync.Mutex
	files   map[string]keptThumbnail
	counter int
	// failKeepAfter makes every Keep after that many succeed fail; 0 never.
	failKeepAfter int
	keeps         int
	// beforeKeep runs before each Keep stores its file.
	beforeKeep func()
}

func newFakeThumbnailFiles() *fakeThumbnailFiles {
	return &fakeThumbnailFiles{files: map[string]keptThumbnail{}}
}

func (fake *fakeThumbnailFiles) store(ownerPrincipalID, photoID string, content []byte) string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.counter++
	id := fmt.Sprintf("file_%06d", fake.counter)
	fake.files[id] = keptThumbnail{ownerPrincipalID: ownerPrincipalID, photoID: photoID, content: append([]byte{}, content...)}
	return id
}

func (fake *fakeThumbnailFiles) Keep(ctx context.Context, ownerPrincipalID, photoID string, jpeg []byte) (string, error) {
	if fake.beforeKeep != nil {
		fake.beforeKeep()
	}
	fake.mutex.Lock()
	fake.keeps++
	failing := fake.failKeepAfter > 0 && fake.keeps > fake.failKeepAfter
	fake.mutex.Unlock()
	if failing {
		return "", errors.New("the fake file-store refuses this file")
	}
	return fake.store(ownerPrincipalID, photoID, jpeg), nil
}

func (fake *fakeThumbnailFiles) KeepCopyOf(ctx context.Context, ownerPrincipalID, photoID, path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fake.store(ownerPrincipalID, photoID, content), nil
}

func (fake *fakeThumbnailFiles) Open(ctx context.Context, fileID string) (io.ReadCloser, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	file, ok := fake.files[fileID]
	if !ok {
		return nil, fmt.Errorf("the fake file-store holds no file %s", fileID)
	}
	return io.NopCloser(bytes.NewReader(file.content)), nil
}

func (fake *fakeThumbnailFiles) Destroy(ctx context.Context, fileID string) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if _, ok := fake.files[fileID]; !ok {
		return fmt.Errorf("the fake file-store holds no file %s", fileID)
	}
	delete(fake.files, fileID)
	return nil
}

// count is how many files the fake holds.
func (fake *fakeThumbnailFiles) count() int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return len(fake.files)
}

// filesOf lists the files kept as one photo's thumbnail.
func (fake *fakeThumbnailFiles) filesOf(ownerPrincipalID, photoID string) []keptThumbnail {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	found := []keptThumbnail{}
	for _, file := range fake.files {
		if file.ownerPrincipalID == ownerPrincipalID && file.photoID == photoID {
			found = append(found, file)
		}
	}
	return found
}

// fakePeople stands in for people-store: which persons each owner keeps.
type fakePeople struct {
	mutex   sync.Mutex
	persons map[string]map[string]bool
	down    bool
}

func (fake *fakePeople) keep(ownerPrincipalID string, personIDs ...string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.persons[ownerPrincipalID] == nil {
		fake.persons[ownerPrincipalID] = map[string]bool{}
	}
	for _, personID := range personIDs {
		fake.persons[ownerPrincipalID][personID] = true
	}
}

func (fake *fakePeople) PersonExists(ctx context.Context, ownerPrincipalID, personID string) (bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.down {
		return false, errors.New("the fake people-store is down")
	}
	return fake.persons[ownerPrincipalID][personID], nil
}
