// Package journalcitation finds the journal-store entry a photo is cited by.
package journalcitation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Store is journal-store, at Base.
type Store struct {
	Base   string
	Client *http.Client
}

// Entry is the part of a journal-store entry a citation needs.
type Entry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// EntryCiting finds the one entry with tag whose text holds token, such as
// a Discord message id or a file name; ErrNotOne when none or several do.
func (journal Store) EntryCiting(tag, token string) (Entry, error) {
	query := url.Values{"tag": {tag}, "q": {`"` + token + `"`}, "limit": {"20"}}
	response, err := journal.Client.Get(journal.Base + "/entries?" + query.Encode())
	if err != nil {
		return Entry{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Entry{}, err
	}
	if response.StatusCode != http.StatusOK {
		return Entry{}, fmt.Errorf("journal-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var found []Entry
	if err := json.Unmarshal(body, &found); err != nil {
		return Entry{}, err
	}
	var matches []Entry
	for _, entry := range found {
		if strings.Contains(entry.Body, token) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return Entry{}, ErrNotOne{Count: len(matches)}
	}
	return matches[0], nil
}

// ErrNotOne says how many entries held the token when it was not one.
type ErrNotOne struct{ Count int }

func (e ErrNotOne) Error() string {
	return fmt.Sprintf("%d entries cite it", e.Count)
}
