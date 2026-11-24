package calendarstore

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PeopleStore asks people-store over HTTP.
type PeopleStore struct {
	BaseURL string
	Client  *http.Client
}

// NewPeopleStore talks to people-store at baseURL.
func NewPeopleStore(baseURL string) *PeopleStore {
	return &PeopleStore{BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: 15 * time.Second}}
}

func (p *PeopleStore) get(path string, into any) (found bool, err error) {
	response, err := p.Client.Get(p.BaseURL + path)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("people-store GET %s: %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	return true, json.Unmarshal(body, into)
}

// PersonByEmail returns the person holding an email address, or "".
func (p *PeopleStore) PersonByEmail(email string) (string, error) {
	var answer struct {
		Identity struct {
			PersonID string `json:"person_id"`
		} `json:"identity"`
	}
	found, err := p.get("/identities/resolve?kind=email&value="+url.QueryEscape(email), &answer)
	if err != nil || !found {
		return "", err
	}
	return answer.Identity.PersonID, nil
}

// PersonExists says whether people-store has the person and has not deleted
// them.
func (p *PeopleStore) PersonExists(personID string) (bool, error) {
	var answer struct {
		ID        string `json:"id"`
		DeletedAt int64  `json:"deleted_at"`
	}
	found, err := p.get("/persons/"+url.PathEscape(personID), &answer)
	if err != nil || !found {
		return false, err
	}
	return answer.DeletedAt == 0, nil
}
