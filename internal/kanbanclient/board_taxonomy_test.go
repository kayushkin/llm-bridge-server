package kanbanclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBoardTaxonomyReadsAsThePrincipalWithoutTheServiceToken(t *testing.T) {
	t.Setenv(ServiceTokenEnvironmentVariable, "service-token")
	var sawPrincipal, sawToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPrincipal, sawToken = r.Header.Get(principalIDHeader), r.Header.Get(ServiceTokenHeader)
		switch r.URL.Path {
		case "/api/boards/board-1":
			if sawToken == "" && sawPrincipal != "principal_000004" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Write([]byte(`{"id":"board-1","updated_at":"2026-09-23T10:00:00Z","taxonomy":{"name":"t","axes":[{"name":"a","values":[{"name":"b"}]}]}}`))
		case "/api/boards/board-bare":
			w.Write([]byte(`{"id":"board-bare","updated_at":"2026-09-23T10:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := New(server.URL)

	taxonomy, version, err := client.BoardTaxonomy(context.Background(), "board-1", "principal_000004")
	if err != nil || taxonomy.Name != "t" || version != "2026-09-23T10:00:00Z" || sawToken != "" || sawPrincipal != "principal_000004" {
		t.Fatalf("as principal: %v %+v %q token=%q principal=%q", err, taxonomy, version, sawToken, sawPrincipal)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-1", "principal_000009"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a principal who may not view the board: %v", err)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-1", ""); err != nil || sawToken != "service-token" {
		t.Fatalf("as the service: %v token=%q", err, sawToken)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-bare", ""); !errors.Is(err, ErrBoardHasNoTaxonomy) {
		t.Fatalf("a board with no taxonomy: %v", err)
	}
}
