// Package principalstore is this repo's client for principal-store, which owns
// who a person is. It exists for one call: turning the address a mail came
// from into the id of a contact, so a ticket points at its requester by id and
// not by a string that changes when the same person writes from their phone.
package principalstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one principal-store.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client with a 5-second timeout: the classifier resolves one
// address per new card, on a tick that already spends minutes in model calls.
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// Contact is the part of principal-store's answer this repo reads.
type Contact struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// ResolveContact turns an email address into one contact id, creating the
// contact the first time that address is seen and finding the same row every
// time after. created is reported so a caller that suddenly makes a contact
// per message can see that it has a normalization bug.
func (c *Client) ResolveContact(email, displayName string) (contact Contact, created bool, err error) {
	payload, err := json.Marshal(map[string]string{"email": email, "display_name": displayName})
	if err != nil {
		return Contact{}, false, err
	}
	response, err := c.HTTP.Post(c.BaseURL+"/contacts/resolve", "application/json", bytes.NewReader(payload))
	if err != nil {
		return Contact{}, false, fmt.Errorf("principal-store did not answer POST /contacts/resolve: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return Contact{}, false, fmt.Errorf("principal-store answered POST /contacts/resolve with %s: %s",
			response.Status, strings.TrimSpace(string(body)))
	}
	var answer struct {
		Contact Contact `json:"contact"`
		Created bool    `json:"created"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Contact{}, false, fmt.Errorf("principal-store answered POST /contacts/resolve with an undecodable body: %w", err)
	}
	if answer.Contact.ID == "" {
		return Contact{}, false, fmt.Errorf("principal-store resolved %q to a contact with no id: %s", email, strings.TrimSpace(string(body)))
	}
	return answer.Contact, answer.Created, nil
}
