package api_test

import (
	"encoding/json"
	"github.com/kayushkin/kanban-store/internal/model"
	"testing"
)

func TestCardPageIncludesReviewedLabelsAndTheirVersion(t *testing.T) {
	f := newClassificationFixture(t)
	category := f.axis("category").ID
	billing := f.value("category", "billing")
	decision := f.publish(f.publication(f.cardID, "op-page", []string{billing}, []string{}), 201)
	accepted, _ := f.review(asPrincipal(activePrincipal), decision.ID, model.ClassificationReviewRequest{IdempotencyKey: "page-review", Action: model.ClassificationReviewActionAccept, AxisIDs: []string{category}}, 201)
	response := f.send(asPrincipal(activePrincipal), "GET", "/api/boards/"+f.boardID+"/card-page?limit=1&offset=0&card_id="+f.cardID, "", nil)
	if response.Code != 200 {
		t.Fatalf("page: %d %s", response.Code, response.Body.String())
	}
	var page model.BoardCardPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Cards) != 1 || page.Cards[0].Classification == nil {
		t.Fatalf("missing projection: %+v", page)
	}
	labels := page.Cards[0].Classification
	if labels.Revision != accepted.Labels.Revision || len(labels.Labels) != 1 || labels.Labels[0].OriginReviewID != accepted.Review.ID || labels.Labels[0].ValueIDs[0] != billing {
		t.Fatalf("lost review provenance: %+v", labels)
	}
}
