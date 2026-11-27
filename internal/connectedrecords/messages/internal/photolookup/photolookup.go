// Package photolookup finds the photo-store photo that holds a picture sent in
// a chat, by the sha256 of the original file: the key photo-store's imports
// upsert by, so a match is the same file and never a guess.
package photolookup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PrivateDocumentTag marks a photo (an ID, a card, a boarding pass) that is
// kept in photo-store but never linked from anywhere else.
const PrivateDocumentTag = "private-document"

// Lookup asks photo-store, and counts what it found.
type Lookup struct {
	storeURL string
	http     *http.Client
	// Found, NotFound and PrivateDocuments count lookups by outcome.
	Found, NotFound, PrivateDocuments int
}

// New makes a lookup against photo-store at storeURL.
func New(storeURL string) *Lookup {
	return &Lookup{storeURL: strings.TrimRight(storeURL, "/"), http: &http.Client{Timeout: time.Minute}}
}

type photo struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
}

// PhotoID is the id of the live photo whose original is these bytes, or ""
// when photo-store holds none, or holds it as a private document.
func (l *Lookup) PhotoID(original []byte) (string, error) {
	digest := sha256.Sum256(original)
	address := l.storeURL + "/photos?sha256=" + url.QueryEscape(hex.EncodeToString(digest[:]))
	response, err := l.http.Get(address)
	if err != nil {
		return "", fmt.Errorf("GET photo-store /photos: %w", err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET photo-store /photos: %s: %s", response.Status, strings.TrimSpace(string(content)))
	}
	var photos []photo
	if err := json.Unmarshal(content, &photos); err != nil {
		return "", fmt.Errorf("photo-store /photos: %w", err)
	}
	if len(photos) == 0 {
		l.NotFound++
		return "", nil
	}
	for _, tag := range photos[0].Tags {
		if tag == PrivateDocumentTag {
			l.PrivateDocuments++
			return "", nil
		}
	}
	l.Found++
	return photos[0].ID, nil
}

// Report is one line for the end of a run.
func (l *Lookup) Report() string {
	return fmt.Sprintf("pictures: %d in photo-store, %d not there, %d left unlinked as private documents", l.Found, l.NotFound, l.PrivateDocuments)
}
