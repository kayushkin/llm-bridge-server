package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// Every name, id and body here is made up for the test.

func supportMailTaxonomy() *msg.ClassificationTaxonomy {
	return &msg.ClassificationTaxonomy{
		Name:   "support-mail",
		Domain: "support mail to a logistics company",
		Axes: []msg.ClassificationAxis{
			{Name: "category", Required: true, Values: []msg.ClassificationValue{
				{Name: "billing", Description: "invoices, refunds, payment failures"},
				{Name: "delivery", Description: "late, lost or damaged shipments"},
			}},
			{Name: "urgency", AllowMultiple: false, Values: []msg.ClassificationValue{
				{Name: "today"}, {Name: "this-week"},
			}},
		},
	}
}

// classificationFixture is a board with a column, a card, an organization,
// a taxonomy and a policy, reached through the principal gate.
type classificationFixture struct {
	t        *testing.T
	h        http.Handler
	grants   *fakeGrantStore
	boardID  string
	columnID string
	cardID   string
	taxonomy msg.ClassificationTaxonomy
	policy   int64
}

func newClassificationFixture(t *testing.T) *classificationFixture {
	t.Helper()
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := &classificationFixture{t: t, h: h, grants: grants}
	f.boardID = mkBoard(t, h, "Support")
	f.columnID = mkColumn(t, h, f.boardID, "Inbox", "")
	f.cardID = f.card("Where is shipment 4471?")
	if w := patchBoard(t, h, f.boardID, model.UpdateBoardRequest{OrganizationID: str(organizationGroup)}); w.Code != 200 {
		t.Fatalf("set organization: %d %s", w.Code, w.Body.String())
	}
	result := f.putTaxonomy(asService, 0, supportMailTaxonomy(), 200)
	f.taxonomy = *result.Classification.Taxonomy
	f.policy = f.putPolicy(asService, 0, map[string]msg.ClassificationAxisPolicy{
		f.axis("category").ID: {Mode: msg.ClassificationAxisModeAssisted},
		f.axis("urgency").ID:  {Mode: msg.ClassificationAxisModeDiscovery},
	}, 200).Policy.Revision
	grants.give(activePrincipal, "can_edit", f.boardID)
	grants.give(otherActivePrincipal, "can_edit", f.boardID)
	grants.give("principal_000004", "can_view", f.boardID)
	return f
}

func (f *classificationFixture) card(title string) string {
	f.t.Helper()
	return createCard(f.t, f.h, f.boardID, f.columnID, title).Placement.CardID
}

func (f *classificationFixture) axis(name string) msg.ClassificationAxis {
	for _, axis := range f.taxonomy.Axes {
		if axis.Name == name {
			return axis
		}
	}
	f.t.Fatalf("no axis %q", name)
	return msg.ClassificationAxis{}
}

func (f *classificationFixture) value(axisName, valueName string) string {
	for _, value := range f.axis(axisName).Values {
		if value.Name == valueName {
			return value.ID
		}
	}
	f.t.Fatalf("no value %q on %q", valueName, axisName)
	return ""
}

func (f *classificationFixture) send(headers map[string]string, method, path, ifMatch string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	withIfMatch := map[string]string{}
	for name, value := range headers {
		withIfMatch[name] = value
	}
	if ifMatch != "" {
		withIfMatch["If-Match"] = ifMatch
	}
	return requestAs(f.t, f.h, withIfMatch, method, path, body)
}

func (f *classificationFixture) path(rest string) string {
	return "/api/boards/" + f.boardID + "/classification" + rest
}

func (f *classificationFixture) putTaxonomy(headers map[string]string, revision int64, taxonomy *msg.ClassificationTaxonomy, want int) model.TaxonomyUpdateResult {
	f.t.Helper()
	w := f.send(headers, "PUT", f.path("/taxonomy"), fmt.Sprintf(`"taxonomy-%d"`, revision), map[string]any{"taxonomy": taxonomy})
	mustStatus(f.t, w, want, "PUT taxonomy")
	var result model.TaxonomyUpdateResult
	if want == 200 {
		decodeSuccessfulResponse(f.t, w, &result)
		if got, expected := w.Header().Get("ETag"), fmt.Sprintf(`"taxonomy-%d"`, result.Classification.TaxonomyRevision); got != expected {
			f.t.Fatalf("ETag %s, want %s", got, expected)
		}
	}
	return result
}

func (f *classificationFixture) putPolicy(headers map[string]string, revision int64, axisPolicies map[string]msg.ClassificationAxisPolicy, want int) msg.BoardClassification {
	f.t.Helper()
	w := f.send(headers, "PUT", f.path("/policy"), fmt.Sprintf(`"policy-%d"`, revision), model.PolicyUpdateRequest{AxisPolicies: axisPolicies})
	mustStatus(f.t, w, want, "PUT policy")
	var result msg.BoardClassification
	if want == 200 {
		decodeSuccessfulResponse(f.t, w, &result)
	}
	return result
}

func (f *classificationFixture) publication(cardID, operationID string, category, urgency []string) msg.BoardClassificationDecisionPublication {
	f.t.Helper()
	received := time.Date(2026, 9, 27, 9, 58, 0, 0, time.UTC)
	source := msg.ClassificationSourceSnapshot{Subject: "Where is shipment 4471?", Body: "Grüße — the order has not arrived.\n" + strings.Repeat("long line ", 500),
		Sender: "customer@example.com", Recipients: []string{"support@example.com"}, ReceivedAt: &received}
	sourceDigest, err := source.Digest()
	if err != nil {
		f.t.Fatal(err)
	}
	taxonomyDigest, err := f.taxonomy.SemanticDigest()
	if err != nil {
		f.t.Fatal(err)
	}
	confidence := 0.7
	return msg.BoardClassificationDecisionPublication{
		BoardID: f.boardID, CardID: cardID, OperationID: operationID, CheckpointID: "checkpoint-1", AttemptNumber: 1,
		InitiatingPrincipalID: activePrincipal, Source: source, SourceDigest: sourceDigest,
		TaxonomyRevision: f.currentTaxonomyRevision(), TaxonomyDigest: taxonomyDigest, PolicyRevision: f.policy, PromptRevision: "classification-prompt-v1",
		Model:      msg.ClassificationModelEvidence{RequestedModel: "efficient", ResolvedModelID: "test-model", Provider: "test-provider"},
		Origin:     msg.ClassificationDecisionOriginModel,
		Selections: map[string][]string{f.axis("category").ID: category, f.axis("urgency").ID: urgency},
		Rationale:  "asks where a shipment is", ModelReportedConfidence: &confidence, CompletedAt: time.Date(2026, 9, 27, 10, 1, 0, 0, time.UTC),
	}
}

func (f *classificationFixture) currentTaxonomyRevision() int64 {
	var classification msg.BoardClassification
	decodeSuccessfulResponse(f.t, f.send(asService, "GET", f.path(""), "", nil), &classification)
	return classification.TaxonomyRevision
}

func (f *classificationFixture) publish(publication msg.BoardClassificationDecisionPublication, want int) model.ClassificationDecision {
	f.t.Helper()
	w := f.send(asService, "POST", f.path("/decisions"), "", publication)
	mustStatus(f.t, w, want, "publish")
	var decision model.ClassificationDecision
	if want == 200 || want == 201 {
		decodeSuccessfulResponse(f.t, w, &decision)
	}
	return decision
}

func (f *classificationFixture) review(headers map[string]string, decisionID string, request model.ClassificationReviewRequest, want int) (model.ClassificationReviewResult, string) {
	f.t.Helper()
	w := f.send(headers, "POST", f.path("/decisions/"+decisionID+"/reviews"), "", request)
	mustStatus(f.t, w, want, "review")
	var result model.ClassificationReviewResult
	if want == 200 || want == 201 {
		decodeSuccessfulResponse(f.t, w, &result)
	}
	return result, w.Body.String()
}

func (f *classificationFixture) labels(cardID string) model.CardClassificationLabels {
	var labels model.CardClassificationLabels
	decodeSuccessfulResponse(f.t, f.send(asService, "GET", "/api/boards/"+f.boardID+"/cards/"+cardID+"/classification", "", nil), &labels)
	return labels
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	return body.Code
}

// ============================ Taxonomy ============================

func TestTheTaxonomyIsWrittenOnlyThroughItsOwnRouteWithIfMatch(t *testing.T) {
	f := newClassificationFixture(t)
	if w := patchBoard(t, f.h, f.boardID, model.UpdateBoardRequest{Taxonomy: supportMailTaxonomy()}); w.Code != 400 || !strings.Contains(w.Body.String(), "/classification/taxonomy") {
		t.Fatalf("PATCH with a taxonomy: want a 400 naming the route, got %d %s", w.Code, w.Body.String())
	}
	if w := f.send(asService, "PUT", f.path("/taxonomy"), "", map[string]any{"taxonomy": supportMailTaxonomy()}); w.Code != 428 {
		t.Fatalf("PUT with no If-Match: want 428, got %d %s", w.Code, w.Body.String())
	}
	w := f.send(asService, "PUT", f.path("/taxonomy"), `"taxonomy-0"`, map[string]any{"taxonomy": supportMailTaxonomy()})
	if w.Code != 412 || errorCode(t, w) != "taxonomy_revision_moved" || !strings.Contains(w.Body.String(), `"current"`) {
		t.Fatalf("PUT against a stale revision: want 412 with the current state, got %d %s", w.Code, w.Body.String())
	}
	if w := f.send(asService, "PUT", f.path("/taxonomy"), `"taxonomy-1"`, map[string]any{"taxonomy": supportMailTaxonomy(), "extra": 1}); w.Code != 400 {
		t.Fatalf("a body with a field the route does not read: want 400, got %d", w.Code)
	}

	board := getBoard(t, f.h, f.boardID)
	if board.TaxonomyRevision != 1 || board.Taxonomy == nil || board.Taxonomy.Axes[0].ID == "" || board.Taxonomy.Axes[0].Values[0].ID == "" {
		t.Fatalf("the board does not carry the taxonomy with ids at revision 1: %+v", board)
	}
	if !strings.HasPrefix(board.Taxonomy.Axes[0].ID, "classification_axis_") || !strings.HasPrefix(board.Taxonomy.Axes[0].Values[0].ID, "classification_value_") {
		t.Fatalf("ids are not the store's: %s %s", board.Taxonomy.Axes[0].ID, board.Taxonomy.Axes[0].Values[0].ID)
	}

	// Editing anything else on the board leaves the revision and digest alone.
	before := f.currentTaxonomyRevision()
	patchBoard(t, f.h, f.boardID, model.UpdateBoardRequest{Name: str("Renamed"), DefaultPrincipalID: str(activePrincipal)})
	if after := getBoard(t, f.h, f.boardID); after.TaxonomyRevision != before {
		t.Fatalf("renaming the board moved the taxonomy revision from %d to %d", before, after.TaxonomyRevision)
	}

	// The same taxonomy again writes nothing.
	same := f.putTaxonomy(asService, 1, &f.taxonomy, 200)
	if same.Classification.TaxonomyRevision != 1 || same.Preview.SemanticChange {
		t.Fatalf("an identical PUT made a revision: %+v", same)
	}
}

func TestTheTaxonomyValidatorAndIdentityRulesRefuse(t *testing.T) {
	f := newClassificationFixture(t)
	other := mkBoard(t, f.h, "Other")
	otherTaxonomy := supportMailTaxonomy()
	otherResult := f.putTaxonomyOn(other, otherTaxonomy)

	edit := func(change func(*msg.ClassificationTaxonomy)) *msg.ClassificationTaxonomy {
		raw, _ := json.Marshal(f.taxonomy)
		var copied msg.ClassificationTaxonomy
		json.Unmarshal(raw, &copied)
		change(&copied)
		return &copied
	}
	for _, testCase := range []struct {
		name, code string
		taxonomy   *msg.ClassificationTaxonomy
	}{
		{"no axes", "taxonomy_invalid", &msg.ClassificationTaxonomy{Name: "named but empty"}},
		{"repeated axis name", "taxonomy_invalid", edit(func(taxonomy *msg.ClassificationTaxonomy) { taxonomy.Axes[1].Name = "category" })},
		{"another board's axis id", "unknown_identifier", edit(func(taxonomy *msg.ClassificationTaxonomy) {
			taxonomy.Axes[0].ID = otherResult.Taxonomy.Axes[0].ID
		})},
		{"a value moved to another axis", "value_moved_axis", edit(func(taxonomy *msg.ClassificationTaxonomy) {
			taxonomy.Axes[1].Values = append(taxonomy.Axes[1].Values, taxonomy.Axes[0].Values[1])
			taxonomy.Axes[0].Values = taxonomy.Axes[0].Values[:1]
		})},
		{"a value left out", "identifier_left_out", edit(func(taxonomy *msg.ClassificationTaxonomy) { taxonomy.Axes[0].Values = taxonomy.Axes[0].Values[:1] })},
		{"an id used twice", "taxonomy_invalid", edit(func(taxonomy *msg.ClassificationTaxonomy) {
			taxonomy.Axes[0].Values = append(taxonomy.Axes[0].Values, msg.ClassificationValue{ID: taxonomy.Axes[0].Values[0].ID, Name: "copy"})
		})},
	} {
		w := f.send(asService, "PUT", f.path("/taxonomy"), `"taxonomy-1"`, map[string]any{"taxonomy": testCase.taxonomy})
		if w.Code != 400 || errorCode(t, w) != testCase.code {
			t.Errorf("%s: want 400 %s, got %d %s", testCase.name, testCase.code, w.Code, w.Body.String())
		}
	}
	if f.currentTaxonomyRevision() != 1 {
		t.Fatal("a refused taxonomy was written")
	}
}

func (f *classificationFixture) putTaxonomyOn(boardID string, taxonomy *msg.ClassificationTaxonomy) msg.BoardClassification {
	f.t.Helper()
	w := f.send(asService, "PUT", "/api/boards/"+boardID+"/classification/taxonomy", `"taxonomy-0"`, map[string]any{"taxonomy": taxonomy})
	var result model.TaxonomyUpdateResult
	decodeSuccessfulResponse(f.t, w, &result)
	return result.Classification
}

func TestRenamingAndArchivingKeepEveryDecisionOnItsOwnTaxonomy(t *testing.T) {
	f := newClassificationFixture(t)
	billing := f.value("category", "billing")
	decision := f.publish(f.publication(f.cardID, "op-1", []string{billing}, []string{}), 201)

	renamed := f.taxonomy
	raw, _ := json.Marshal(f.taxonomy)
	json.Unmarshal(raw, &renamed)
	renamed.Axes[0].Values[0].Name = "invoices"
	renamed.Axes[0].Values = append(renamed.Axes[0].Values, msg.ClassificationValue{Name: "customs"})
	result := f.putTaxonomy(asService, 1, &renamed, 200)
	if result.Classification.TaxonomyRevision != 2 || !result.Preview.SemanticChange || result.Preview.DecisionsUnderOtherTaxonomies != 1 {
		t.Fatalf("preview after a rename: %+v", result)
	}
	if result.Classification.Taxonomy.Axes[0].Values[0].ID != billing || result.Classification.Taxonomy.Axes[0].Values[2].ID == "" {
		t.Fatalf("the renamed value lost its id, or the new one got none: %+v", result.Classification.Taxonomy.Axes[0].Values)
	}

	var detail model.ClassificationDecisionDetail
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions/"+decision.ID), "", nil), &detail)
	if detail.Decision.TaxonomyRevision != 1 || detail.Decision.Taxonomy == nil || detail.Decision.Taxonomy.Axes[0].Values[0].Name != "billing" {
		t.Fatalf("the decision does not keep the taxonomy it was made under: %+v", detail.Decision.Taxonomy)
	}
	if detail.Decision.Selections[f.axis("category").ID][0] != billing {
		t.Fatalf("the decision's selection moved: %+v", detail.Decision.Selections)
	}

	// Archive billing: the decision still resolves, and nobody can pick it now.
	f.taxonomy = *result.Classification.Taxonomy
	archived := f.taxonomy
	json.Unmarshal(func() []byte { raw, _ := json.Marshal(f.taxonomy); return raw }(), &archived)
	archived.Axes[0].Values[0].Archived = true
	f.putTaxonomy(asService, 2, &archived, 200)
	_, body := f.review(asPrincipal(activePrincipal), decision.ID, model.ClassificationReviewRequest{
		IdempotencyKey: "accept-archived", Action: model.ClassificationReviewActionAccept, AxisIDs: []string{f.axis("category").ID},
	}, 409)
	if !strings.Contains(body, "taxonomy_incompatible") {
		t.Fatalf("accepting an archived value: %s", body)
	}
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions/"+decision.ID), "", nil), &detail)
	if detail.Decision.Taxonomy.Axes[0].Values[0].Name != "billing" || detail.Decision.Taxonomy.Axes[0].Values[0].Archived {
		t.Fatal("archiving changed the taxonomy the decision was made under")
	}
}

func TestClearingTheTaxonomyWaitsForThePolicy(t *testing.T) {
	f := newClassificationFixture(t)
	if w := f.send(asService, "PUT", f.path("/taxonomy"), `"taxonomy-1"`, map[string]any{"taxonomy": nil}); w.Code != 409 || errorCode(t, w) != "policy_uses_taxonomy" {
		t.Fatalf("clearing under a live policy: want 409, got %d %s", w.Code, w.Body.String())
	}
	f.putPolicy(asService, f.policy, map[string]msg.ClassificationAxisPolicy{}, 200)
	result := f.putTaxonomy(asService, 1, nil, 200)
	if result.Classification.Taxonomy != nil || result.Classification.TaxonomyRevision != 2 {
		t.Fatalf("clear: %+v", result.Classification)
	}
	if board := getBoard(t, f.h, f.boardID); board.Taxonomy != nil || board.TaxonomyRevision != 2 {
		t.Fatalf("the board still carries a cleared taxonomy: %+v", board)
	}
}

// ============================ Policy ============================

func TestThePolicyIsRevisionedAndHeldToTheTaxonomy(t *testing.T) {
	f := newClassificationFixture(t)
	var classification msg.BoardClassification
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path(""), "", nil), &classification)
	if classification.Policy == nil || classification.Policy.Revision != 1 || len(classification.SupportedModes) != 3 {
		t.Fatalf("GET classification: %+v", classification)
	}
	for _, mode := range classification.SupportedModes {
		if mode == "auto" {
			t.Fatal("auto is offered")
		}
	}
	if w := f.send(asService, "PUT", f.path("/policy"), "", model.PolicyUpdateRequest{AxisPolicies: map[string]msg.ClassificationAxisPolicy{}}); w.Code != 428 {
		t.Fatalf("no If-Match: want 428, got %d", w.Code)
	}
	f.putPolicy(asService, 0, map[string]msg.ClassificationAxisPolicy{}, 412)
	f.putPolicy(asService, 1, map[string]msg.ClassificationAxisPolicy{f.axis("category").ID: {Mode: "auto"}}, 400)
	f.putPolicy(asService, 1, map[string]msg.ClassificationAxisPolicy{"classification_axis_999999": {Mode: msg.ClassificationAxisModeOff}}, 400)
	same := f.putPolicy(asService, 1, classification.Policy.AxisPolicies, 200)
	if same.Policy.Revision != 1 {
		t.Fatalf("an identical policy made revision %d", same.Policy.Revision)
	}
	changed := f.putPolicy(asService, 1, map[string]msg.ClassificationAxisPolicy{f.axis("category").ID: {Mode: msg.ClassificationAxisModeOff}}, 200)
	if changed.Policy.Revision != 2 || changed.Policy.ModeOf(f.axis("urgency").ID) != msg.ClassificationAxisModeOff {
		t.Fatalf("changed policy: %+v", changed.Policy)
	}

	// A board with no taxonomy has no policy to set, and says it is unconfigured.
	bare := mkBoard(t, f.h, "Bare")
	var unconfigured msg.BoardClassification
	decodeSuccessfulResponse(t, f.send(asService, "GET", "/api/boards/"+bare+"/classification", "", nil), &unconfigured)
	if unconfigured.Policy != nil || unconfigured.Taxonomy != nil || unconfigured.TaxonomyRevision != 0 {
		t.Fatalf("a bare board was given defaults: %+v", unconfigured)
	}
	if w := f.send(asService, "PUT", "/api/boards/"+bare+"/classification/policy", `"policy-0"`, model.PolicyUpdateRequest{AxisPolicies: map[string]msg.ClassificationAxisPolicy{}}); w.Code != 409 {
		t.Fatalf("policy on a board with no taxonomy: want 409, got %d %s", w.Code, w.Body.String())
	}
}

// ============================ Who may do what ============================

func TestEachClassificationActionHasItsOwnGate(t *testing.T) {
	f := newClassificationFixture(t)
	viewer, editor, stranger := asPrincipal("principal_000004"), asPrincipal(activePrincipal), asPrincipal("principal_000005")
	f.grants.give("principal_000005", "can_view", mkBoard(t, f.h, "Elsewhere"))

	var classification msg.BoardClassification
	decodeSuccessfulResponse(t, f.send(viewer, "GET", f.path(""), "", nil), &classification)
	if fmt.Sprint(classification.CallerActions) != "[read]" {
		t.Fatalf("a viewer's actions: %v", classification.CallerActions)
	}
	decodeSuccessfulResponse(t, f.send(editor, "GET", f.path(""), "", nil), &classification)
	if fmt.Sprint(classification.CallerActions) != "[read classify review]" {
		t.Fatalf("an editor's actions: %v", classification.CallerActions)
	}
	if w := f.send(stranger, "GET", f.path(""), "", nil); w.Code != 404 {
		t.Fatalf("a principal who cannot see the board: want 404, got %d", w.Code)
	}
	if w := f.send(editor, "PUT", f.path("/taxonomy"), `"taxonomy-1"`, map[string]any{"taxonomy": f.taxonomy}); w.Code != 403 {
		t.Fatalf("an editor changing the taxonomy: want 403, got %d", w.Code)
	}
	if w := f.send(editor, "PUT", f.path("/policy"), `"policy-1"`, model.PolicyUpdateRequest{AxisPolicies: map[string]msg.ClassificationAxisPolicy{}}); w.Code != 403 {
		t.Fatalf("an editor changing the policy: want 403, got %d", w.Code)
	}

	publication := f.publication(f.cardID, "op-gate", []string{f.value("category", "billing")}, []string{})
	for name, headers := range map[string]map[string]string{"an editor": editor, "an administrator": asPrincipal(deploymentAdministrator)} {
		if w := f.send(headers, "POST", f.path("/decisions"), "", publication); w.Code != 403 {
			t.Fatalf("%s publishing a decision: want 403, got %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := f.send(stranger, "POST", f.path("/decisions"), "", publication); w.Code != 404 {
		t.Fatalf("a stranger publishing: want 404, got %d", w.Code)
	}
	decision := f.publish(publication, 201)

	review := model.ClassificationReviewRequest{IdempotencyKey: "k", Action: model.ClassificationReviewActionAccept, AxisIDs: []string{f.axis("category").ID}}
	f.review(viewer, decision.ID, review, 403)
	f.review(stranger, decision.ID, review, 404)
	if _, body := f.review(asService, decision.ID, review, 403); !strings.Contains(body, "reviewer_required") {
		t.Fatalf("the service token reviewing: %s", body)
	}
	if w := f.send(viewer, "GET", f.path("/decisions/"+decision.ID), "", nil); w.Code != 200 {
		t.Fatalf("a viewer reading a decision: %d", w.Code)
	}
	// A decision id from this board read through another board is not found.
	if w := f.send(asService, "GET", "/api/boards/"+mkBoard(t, f.h, "Third")+"/classification/decisions/"+decision.ID, "", nil); w.Code != 404 {
		t.Fatalf("a decision read through another board: want 404, got %d", w.Code)
	}
}

// ============================ Publication ============================

func TestPublicationIsCheckedAndIdempotent(t *testing.T) {
	f := newClassificationFixture(t)
	billing := f.value("category", "billing")
	publication := f.publication(f.cardID, "op-1", []string{billing}, []string{})

	first := f.publish(publication, 201)
	if first.OrganizationID != organizationGroup || first.Source != nil || first.PublicationKey == "" {
		t.Fatalf("published decision: %+v", first)
	}
	again := f.publish(publication, 200)
	if again.ID != first.ID {
		t.Fatalf("the same publication twice made two decisions: %s and %s", first.ID, again.ID)
	}
	different := publication
	different.Selections = map[string][]string{f.axis("category").ID: {f.value("category", "delivery")}, f.axis("urgency").ID: {}}
	if w := f.send(asService, "POST", f.path("/decisions"), "", different); w.Code != 409 || errorCode(t, w) != "publication_key_reused" {
		t.Fatalf("a different answer under the same key: want 409, got %d %s", w.Code, w.Body.String())
	}

	spoiled := func(change func(*msg.BoardClassificationDecisionPublication)) msg.BoardClassificationDecisionPublication {
		copied := f.publication(f.cardID, "op-2", []string{billing}, []string{})
		change(&copied)
		return copied
	}
	for _, testCase := range []struct {
		name, code  string
		status      int
		publication msg.BoardClassificationDecisionPublication
	}{
		{"source edited after digesting", "publication_invalid", 400, spoiled(func(p *msg.BoardClassificationDecisionPublication) { p.Source.Body += "!" })},
		{"a taxonomy digest from elsewhere", "taxonomy_digest_mismatch", 409, spoiled(func(p *msg.BoardClassificationDecisionPublication) {
			p.TaxonomyDigest = "classification-taxonomy-v1:sha256:00"
		})},
		{"a policy revision that does not exist", "policy_revision_unknown", 409, spoiled(func(p *msg.BoardClassificationDecisionPublication) { p.PolicyRevision = 9 })},
		{"a card on no board of this", "card_not_on_board", 409, spoiled(func(p *msg.BoardClassificationDecisionPublication) { p.CardID = "card-elsewhere" })},
		{"an unknown value", "publication_invalid", 400, spoiled(func(p *msg.BoardClassificationDecisionPublication) {
			p.Selections[f.axis("category").ID] = []string{"classification_value_999999"}
		})},
	} {
		w := f.send(asService, "POST", f.path("/decisions"), "", testCase.publication)
		if w.Code != testCase.status || errorCode(t, w) != testCase.code {
			t.Errorf("%s: want %d %s, got %d %s", testCase.name, testCase.status, testCase.code, w.Code, w.Body.String())
		}
	}

	offPolicy := f.putPolicy(asService, f.policy, map[string]msg.ClassificationAxisPolicy{f.axis("category").ID: {Mode: msg.ClassificationAxisModeAssisted}}, 200)
	f.policy = offPolicy.Policy.Revision
	if w := f.send(asService, "POST", f.path("/decisions"), "", f.publication(f.cardID, "op-3", []string{billing}, []string{})); w.Code != 400 || errorCode(t, w) != "axis_off" {
		t.Fatalf("publishing on an axis that is off: want 400 axis_off, got %d %s", w.Code, w.Body.String())
	}

	// The initiating principal must still be able to classify when it publishes.
	withoutUrgency := f.publication(f.cardID, "op-4", []string{billing}, nil)
	delete(withoutUrgency.Selections, f.axis("urgency").ID)
	withoutUrgency.InitiatingPrincipalID = "principal_000004"
	if w := f.send(asService, "POST", f.path("/decisions"), "", withoutUrgency); w.Code != 403 || errorCode(t, w) != "initiating_principal_may_not_classify" {
		t.Fatalf("a viewer as initiating principal: want 403, got %d %s", w.Code, w.Body.String())
	}

	// A board with no organization publishes nothing.
	patchBoard(t, f.h, f.boardID, model.UpdateBoardRequest{OrganizationID: str("")})
	withoutUrgency.InitiatingPrincipalID = activePrincipal
	if w := f.send(asService, "POST", f.path("/decisions"), "", withoutUrgency); w.Code != 409 || errorCode(t, w) != "board_has_no_organization" {
		t.Fatalf("a board with no organization: want 409, got %d %s", w.Code, w.Body.String())
	}
	if w := patchBoard(t, f.h, f.boardID, model.UpdateBoardRequest{OrganizationID: str(activePrincipal)}); w.Code != 400 {
		t.Fatalf("a person as the organization: want 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestTheFullSourceSurvivesAndGoesWithAPurgedCard(t *testing.T) {
	f := newClassificationFixture(t)
	publication := f.publication(f.cardID, "op-1", []string{f.value("category", "billing")}, []string{})
	decision := f.publish(publication, 201)
	var detail model.ClassificationDecisionDetail
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions/"+decision.ID), "", nil), &detail)
	if detail.Decision.Source == nil || detail.Decision.Source.Body != publication.Source.Body {
		t.Fatal("the decision does not return the text it was given, byte for byte")
	}
	if w := do(t, f.h, "DELETE", "/api/cards/"+f.cardID+"?hard=true", nil); w.Code != 200 {
		t.Fatalf("purge: %d %s", w.Code, w.Body.String())
	}
	var purged model.ClassificationDecisionDetail
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions/"+decision.ID), "", nil), &purged)
	if purged.Decision.Source != nil || purged.Decision.SourceRemovedAt == nil || purged.Decision.SourceDigest != publication.SourceDigest {
		t.Fatalf("after the card was purged: source present %v, removed at %v, digest %s", purged.Decision.Source != nil, purged.Decision.SourceRemovedAt, purged.Decision.SourceDigest)
	}
}

// ============================ Reviews and labels ============================

func TestOnlyAPersonsReviewSetsALabelAndNoCorrectionIsOverwrittenUnseen(t *testing.T) {
	f := newClassificationFixture(t)
	category := f.axis("category").ID
	billing, delivery := f.value("category", "billing"), f.value("category", "delivery")
	first := f.publish(f.publication(f.cardID, "op-1", []string{billing}, []string{}), 201)
	if labels := f.labels(f.cardID); labels.Revision != 0 || len(labels.Labels) != 0 {
		t.Fatalf("a decision alone set labels: %+v", labels)
	}

	accept := model.ClassificationReviewRequest{IdempotencyKey: "accept-1", Action: model.ClassificationReviewActionAccept, AxisIDs: []string{category}}
	accepted, _ := f.review(asPrincipal(activePrincipal), first.ID, accept, 201)
	if accepted.Labels.Revision != 1 || accepted.Labels.Labels[0].ValueIDs[0] != billing || accepted.Labels.Labels[0].OriginReviewID != accepted.Review.ID {
		t.Fatalf("accept: %+v", accepted)
	}
	replay, _ := f.review(asPrincipal(activePrincipal), first.ID, accept, 200)
	if replay.Review.ID != accepted.Review.ID || replay.Labels.Revision != 1 {
		t.Fatalf("a replayed review: %+v", replay)
	}
	changedBody := accept
	changedBody.Explanation = "second thoughts"
	if _, body := f.review(asPrincipal(activePrincipal), first.ID, changedBody, 409); !strings.Contains(body, "idempotency_key_reused") {
		t.Fatalf("the same key with another body: %s", body)
	}

	// A newer answer is a proposal; the reviewed label stays.
	second := f.publish(f.publication(f.cardID, "op-2", []string{delivery}, []string{}), 201)
	if labels := f.labels(f.cardID); labels.Revision != 1 || labels.Labels[0].ValueIDs[0] != billing {
		t.Fatalf("a newer decision replaced a reviewed label: %+v", labels)
	}

	// Another person accepting it must have seen the first review.
	acceptSecond := model.ClassificationReviewRequest{IdempotencyKey: "accept-2", Action: model.ClassificationReviewActionAccept, AxisIDs: []string{category}, ExpectedLabelsRevision: 0}
	if _, body := f.review(asPrincipal(otherActivePrincipal), second.ID, acceptSecond, 409); !strings.Contains(body, "labels_revision_moved") {
		t.Fatalf("a review against a stale labels revision: %s", body)
	}
	acceptSecond.ExpectedLabelsRevision = 1
	if _, body := f.review(asPrincipal(otherActivePrincipal), second.ID, acceptSecond, 409); !strings.Contains(body, "supersedes_required") {
		t.Fatalf("replacing a person's label without naming their review: %s", body)
	}
	acceptSecond.SupersedesReviewIDs = []string{accepted.Review.ID}
	superseding, _ := f.review(asPrincipal(otherActivePrincipal), second.ID, acceptSecond, 201)
	if superseding.Labels.Revision != 2 || superseding.Labels.Labels[0].ValueIDs[0] != delivery {
		t.Fatalf("superseding review: %+v", superseding)
	}

	// Reject changes no label and needs no supersedes.
	rejected, _ := f.review(asPrincipal(activePrincipal), first.ID, model.ClassificationReviewRequest{IdempotencyKey: "reject-1",
		Action: model.ClassificationReviewActionReject, AxisIDs: []string{category}, ExpectedLabelsRevision: 2}, 201)
	if rejected.Labels.Revision != 2 || rejected.Review.ResultingLabelsRevision != 2 {
		t.Fatalf("reject moved the labels: %+v", rejected)
	}

	// A correction must stay inside the taxonomy and the axis's cardinality.
	correct := model.ClassificationReviewRequest{IdempotencyKey: "correct-1", Action: model.ClassificationReviewActionCorrect, AxisIDs: []string{category},
		Corrections: map[string][]string{category: {billing, delivery}}, ExpectedLabelsRevision: 2, SupersedesReviewIDs: []string{superseding.Review.ID}}
	f.review(asPrincipal(activePrincipal), first.ID, correct, 409)
	correct.IdempotencyKey, correct.Corrections = "correct-2", map[string][]string{category: {}}
	f.review(asPrincipal(activePrincipal), first.ID, correct, 400)

	// An axis in discovery takes no reviews.
	discovery := model.ClassificationReviewRequest{IdempotencyKey: "discovery", Action: model.ClassificationReviewActionReject, AxisIDs: []string{f.axis("urgency").ID}, ExpectedLabelsRevision: 2}
	if _, body := f.review(asPrincipal(activePrincipal), first.ID, discovery, 409); !strings.Contains(body, "axis_not_assisted") {
		t.Fatalf("reviewing a discovery axis: %s", body)
	}

	var detail model.ClassificationDecisionDetail
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions/"+first.ID), "", nil), &detail)
	if len(detail.Reviews) != 2 || detail.Decision.ReviewCount != 2 || detail.Decision.Selections[category][0] != billing || detail.Labels.Revision != 2 {
		t.Fatalf("decision detail after reviews: %+v", detail)
	}
}

func TestTwoReviewersRacingOnOneRevisionLeaveOneWinner(t *testing.T) {
	f := newClassificationFixture(t)
	category := f.axis("category").ID
	decision := f.publish(f.publication(f.cardID, "op-1", []string{f.value("category", "billing")}, []string{}), 201)
	statuses := make(chan int, 8)
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			reviewer := activePrincipal
			if index%2 == 1 {
				reviewer = otherActivePrincipal
			}
			w := requestAs(t, f.h, asPrincipal(reviewer), "POST", f.path("/decisions/"+decision.ID+"/reviews"), model.ClassificationReviewRequest{
				IdempotencyKey: fmt.Sprintf("race-%d", index), Action: model.ClassificationReviewActionCorrect, AxisIDs: []string{category},
				Corrections: map[string][]string{category: {f.value("category", "delivery")}}, ExpectedLabelsRevision: 0})
			statuses <- w.Code
		}(index)
	}
	wait.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[201] != 1 || counts[409] != 7 {
		t.Fatalf("eight reviews against revision 0: %v, want one 201 and seven 409", counts)
	}
	if labels := f.labels(f.cardID); labels.Revision != 1 {
		t.Fatalf("labels revision after the race: %d", labels.Revision)
	}
}

// ============================ Listing ============================

func TestDecisionsArePagedAndFilteredWithoutTheirText(t *testing.T) {
	f := newClassificationFixture(t)
	billing, delivery := f.value("category", "billing"), f.value("category", "delivery")
	secondCard := f.card("Invoice 88 is wrong")
	for index := 0; index < 5; index++ {
		f.publish(f.publication(f.cardID, fmt.Sprintf("op-%d", index), []string{billing}, []string{}), 201)
	}
	flagged := f.publication(secondCard, "op-flagged", []string{delivery}, []string{})
	flagged.NeedsReview = true
	flagged.ReviewReasons = []msg.OperationEvidence{{Kind: "required_axis_empty", Summary: "made up"}}
	f.publish(flagged, 201)

	var page model.ClassificationDecisionPage
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions?limit=4"), "", nil), &page)
	if page.Total != 6 || len(page.Decisions) != 4 || page.NextCursor == "" {
		t.Fatalf("first page: total %d, %d decisions, cursor %q", page.Total, len(page.Decisions), page.NextCursor)
	}
	if page.Decisions[0].CardID != secondCard {
		t.Fatal("the newest decision is not first")
	}
	for _, decision := range page.Decisions {
		if decision.Source != nil || decision.Taxonomy != nil {
			t.Fatal("a list carries source text or a taxonomy")
		}
	}
	seen := map[string]bool{}
	for _, decision := range page.Decisions {
		seen[decision.ID] = true
	}
	var rest model.ClassificationDecisionPage
	decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions?limit=4&cursor="+page.NextCursor), "", nil), &rest)
	if len(rest.Decisions) != 2 || rest.NextCursor != "" || rest.Total != 6 {
		t.Fatalf("second page: %+v", rest)
	}
	for _, decision := range rest.Decisions {
		if seen[decision.ID] {
			t.Fatalf("decision %s on both pages", decision.ID)
		}
	}

	other := mkBoard(t, f.h, "Other")
	if w := f.send(asService, "GET", "/api/boards/"+other+"/classification/decisions?cursor="+page.NextCursor, "", nil); w.Code != 400 {
		t.Fatalf("a cursor from another board: want 400, got %d", w.Code)
	}
	for query, want := range map[string]int{
		"card_id=" + secondCard:                                      1,
		"review_state=needs_review":                                  1,
		"review_state=unreviewed":                                    6,
		"axis_id=" + f.axis("category").ID + "&value_id=" + delivery: 1,
		"operation_id=op-3":                                          1,
	} {
		var filtered model.ClassificationDecisionPage
		decodeSuccessfulResponse(t, f.send(asService, "GET", f.path("/decisions?"+query), "", nil), &filtered)
		if filtered.Total != want || len(filtered.Decisions) != want {
			t.Errorf("%s: total %d, %d decisions, want %d", query, filtered.Total, len(filtered.Decisions), want)
		}
	}
	for _, query := range []string{"review_state=maybe", "value_id=" + billing, "limit=0", "limit=101"} {
		if w := f.send(asService, "GET", f.path("/decisions?"+query), "", nil); w.Code != 400 {
			t.Errorf("%s: want 400, got %d", query, w.Code)
		}
	}
}
