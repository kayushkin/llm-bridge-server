package kanbanclient

import (
	"context"
	"errors"
	"net/http"

	kanbanapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/api"
	"testing"
)

func TestBoardTaxonomyReadsAsThePrincipalNotTheInternalService(t *testing.T) {
	var sawCaller kanbanapi.Caller
	client := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCaller, _ = kanbanapi.CallerFrom(r.Context())
		switch r.URL.Path {
		case "/api/boards/board-1/classification":
			if !sawCaller.InternalService && sawCaller.PrincipalID != "principal_000004" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Write([]byte(`{"board_id":"board-1","taxonomy_revision":3,"taxonomy":{"name":"t","axes":[{"id":"classification_axis_000001","name":"a","values":[{"id":"classification_value_000002","name":"b"},{"id":"classification_value_000003","name":"retired","archived":true}]}]},"policy":null,"supported_modes":["off"],"caller_actions":["read"]}`))
		case "/api/boards/board-bare/classification":
			w.Write([]byte(`{"board_id":"board-bare","taxonomy":null,"taxonomy_revision":0,"policy":null,"supported_modes":["off"],"caller_actions":["read"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	taxonomy, version, err := client.BoardTaxonomy(context.Background(), "board-1", "principal_000004")
	if err != nil || taxonomy.Name != "t" || version != "3" || len(taxonomy.Axes[0].Values) != 2 || sawCaller != (kanbanapi.Caller{PrincipalID: "principal_000004"}) {
		t.Fatalf("as principal: %v %+v %q caller=%+v", err, taxonomy, version, sawCaller)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-1", "principal_000009"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a principal who may not view the board: %v", err)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-1", ""); err != nil || !sawCaller.InternalService {
		t.Fatalf("as the service: %v caller=%+v", err, sawCaller)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-bare", ""); !errors.Is(err, ErrBoardHasNoTaxonomy) {
		t.Fatalf("a board with no taxonomy: %v", err)
	}
}
