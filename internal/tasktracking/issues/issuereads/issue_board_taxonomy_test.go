package issuereads

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads/issuereadstest"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/executors"
	issuesapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/api"
	"github.com/kayushkin/llm-bridge/msg"
)

func isIssueNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func TestBoardTaxonomyReadsAsThePrincipalNotTheInternalService(t *testing.T) {
	var sawCaller issuesapi.Caller
	store := &issuereadstest.Store{BoardClassificationAnswer: func(caller issuesapi.Caller, boardID string) (*msg.BoardClassification, error) {
		sawCaller = caller
		switch boardID {
		case "board-1":
			if !caller.InternalService && caller.PrincipalID != "principal_000004" {
				return nil, &issuesapi.Refusal{Status: http.StatusForbidden, Message: "needs can_view"}
			}
			return &msg.BoardClassification{BoardID: "board-1", TaxonomyRevision: 3, Taxonomy: &msg.ClassificationTaxonomy{Name: "t", Axes: []msg.ClassificationAxis{{
				ID: "classification_axis_000001", Name: "a", Values: []msg.ClassificationValue{
					{ID: "classification_value_000002", Name: "b"}, {ID: "classification_value_000003", Name: "retired", Archived: true}}}}}}, nil
		case "board-bare":
			return &msg.BoardClassification{BoardID: "board-bare"}, nil
		}
		return nil, &issuesapi.Refusal{Status: http.StatusNotFound, Message: "not found"}
	}}
	client := New(store)

	taxonomy, version, err := client.BoardTaxonomy(context.Background(), "board-1", "principal_000004")
	if err != nil || taxonomy.Name != "t" || version != "3" || len(taxonomy.Axes[0].Values) != 2 || sawCaller != (issuesapi.Caller{PrincipalID: "principal_000004"}) {
		t.Fatalf("as principal: %v %+v %q caller=%+v", err, taxonomy, version, sawCaller)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-1", "principal_000009"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a principal who may not view the board: %v", err)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-1", ""); err != nil || !sawCaller.InternalService {
		t.Fatalf("as the service: %v caller=%+v", err, sawCaller)
	}
	if _, _, err := client.BoardTaxonomy(context.Background(), "board-bare", ""); !errors.Is(err, executors.ErrBoardHasNoTaxonomy) {
		t.Fatalf("a board with no taxonomy: %v", err)
	}
}
