package projectstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestStore is a store on a Postgres database of the test's own.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}

// httpNoteboard is Noteboard over a fake noteboard's HTTP routes, as the
// standalone store called noteboard before both moved into one process.
type httpNoteboard struct {
	client *http.Client
	url    string
}

func (noteboard httpNoteboard) call(ctx context.Context, method, path string, body any, wantStatus int, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, noteboard.url+path, reader)
	if err != nil {
		return err
	}
	response, err := noteboard.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOwnerUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && wantStatus == http.StatusOK && method == http.MethodGet {
		return ErrNotFound
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%w: %s %s: %s", ErrOwnerUnavailable, method, path, response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (noteboard httpNoteboard) ItemTitle(ctx context.Context, id string) (string, error) {
	var item NoteboardItem
	err := noteboard.call(ctx, http.MethodGet, "/api/items/"+id, nil, http.StatusOK, &item)
	return item.Title, err
}

func (noteboard httpNoteboard) LinkedItems(ctx context.Context, ids []string) ([]NoteboardItem, []string, error) {
	var answer struct {
		Items      []NoteboardItem `json:"items"`
		MissingIDs []string        `json:"missing_ids"`
	}
	err := noteboard.call(ctx, http.MethodPost, "/api/items/query", map[string]any{"ids": ids, "include_items": true, "limit": len(ids)}, http.StatusOK, &answer)
	return answer.Items, answer.MissingIDs, err
}

func (noteboard httpNoteboard) CreateTodo(ctx context.Context, title, body string, tags []string, priority *int) (NoteboardItem, error) {
	todo := map[string]any{"type": "todo", "title": title, "body": body, "tags": tags}
	if priority != nil {
		todo["priority"] = *priority
	}
	var item NoteboardItem
	err := noteboard.call(ctx, http.MethodPost, "/api/items", todo, http.StatusCreated, &item)
	return item, err
}

// httpPrincipals answers Principals from a fake principal-store's
// GET /principals/{id}, so the tests' fake owners serve principals as before.
type httpPrincipals struct {
	client *http.Client
	url    string
}

func (principals httpPrincipals) DisplayName(ctx context.Context, id string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, principals.url+"/principals/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	response, err := principals.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrOwnerUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: principal-store answered %d", ErrOwnerUnavailable, response.StatusCode)
	}
	var principal struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&principal); err != nil {
		return "", err
	}
	return principal.DisplayName, nil
}
