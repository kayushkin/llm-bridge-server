package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

// doJSON issues one request and insists on an expected status. Anything else is
// an error carrying the body, because a silent non-2xx here means a card that
// looks promoted and is not.
func doJSON(method, url string, body any, okStatuses ...int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range okStatuses {
		if resp.StatusCode == want {
			return nil
		}
	}
	return fmt.Errorf("%s %s returned %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(raw)))
}

// patchItem edits the noteboard item behind a card, through kanban-store, which
// forwards the patch unchanged.
func patchItem(kanbanURL, cardID string, patch map[string]any) error {
	return doJSON(http.MethodPatch,
		fmt.Sprintf("%s/api/cards/%s", strings.TrimRight(kanbanURL, "/"), cardID),
		patch, http.StatusOK)
}

// addNote writes a note onto the card's timeline, attributed.
func addNote(kanbanURL, cardID, boardID, actor, body string) error {
	return doJSON(http.MethodPost,
		fmt.Sprintf("%s/api/cards/%s/notes", strings.TrimRight(kanbanURL, "/"), cardID),
		map[string]any{"body": body, "board_id": boardID, "kind": "status", "actor": actor},
		http.StatusCreated, http.StatusOK)
}

// linkAs creates a card link and says who created it.
//
// kanban-store reads the actor from the query string (body actor wins, else
// ?actor=), and kanbanclient's AddCardLink has nowhere to put one — so the
// events its links generate have always been anonymous. This is the same call
// with attribution attached.
func linkAs(kanbanURL, cardID, entityType, entityRef, label, actor string) error {
	url := fmt.Sprintf("%s/api/cards/%s/links?actor=%s",
		strings.TrimRight(kanbanURL, "/"), cardID, neturl.QueryEscape(actor))
	return doJSON(http.MethodPost, url, map[string]any{
		"entity_type": entityType, "entity_ref": entityRef, "label": label,
	}, http.StatusCreated, http.StatusOK)
}

// getItem reads a card's noteboard item.
//
// Straight from noteboard, because kanban-store has no single-card read — its
// /api/cards/{id} answers PATCH and DELETE and 405s everything else. A card id
// IS a noteboard item id, so this is the same record by its owner's route.
func getItem(noteboardURL, cardID string) (map[string]any, error) {
	url := fmt.Sprintf("%s/api/items/%s", strings.TrimRight(noteboardURL, "/"), cardID)
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("read item %s: %w", cardID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("read item %s: %d %s", cardID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var item map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, fmt.Errorf("decode item %s: %w", cardID, err)
	}
	return item, nil
}

// retag swaps one tag for another, preserving the rest.
//
// It reads the card back rather than trusting a copy held from earlier in the
// tick, because a tags patch REPLACES the whole list: writing a stale one
// silently drops whatever the classifier added in between. An error is returned
// rather than an empty list, since patching nil tags would erase them all.
func retag(noteboardURL, cardID, remove, add string) ([]string, error) {
	item, err := getItem(noteboardURL, cardID)
	if err != nil {
		return nil, err
	}
	out := []string{add}
	for _, t := range tagsOf(item) {
		if t != remove && t != add {
			out = append(out, t)
		}
	}
	return out, nil
}
