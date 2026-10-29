package executors_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/executors/executorstest"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

type decisionStore struct {
	settings      msg.BoardClassification
	source        json.RawMessage
	publications  []msg.BoardClassificationDecisionPublication
	publishErr    error
	beforePublish func()
}

func (s *decisionStore) ClassificationSettings(_ context.Context, _ string, principal string) (msg.BoardClassification, error) {
	if principal != "person" {
		return msg.BoardClassification{}, errors.New("wrong principal")
	}
	return s.settings, nil
}
func (s *decisionStore) ClassificationCard(_ context.Context, _, _, principal string) (json.RawMessage, error) {
	if principal != "person" {
		return nil, errors.New("wrong principal")
	}
	return s.source, nil
}
func (s *decisionStore) PublishClassification(_ context.Context, p msg.BoardClassificationDecisionPublication, h http.Header) (json.RawMessage, error) {
	if s.beforePublish != nil {
		s.beforePublish()
	}
	if h.Get(operations.OperationIDHeader) == "" {
		return nil, errors.New("missing operation correlation")
	}
	s.publications = append(s.publications, p)
	return json.RawMessage(`{"id":"decision-1"}`), s.publishErr
}
func boardFixture() *decisionStore {
	taxonomy := msg.ClassificationTaxonomy{Name: "Support", Axes: []msg.ClassificationAxis{{ID: "axis-1", Name: "Topic", Required: true, Values: []msg.ClassificationValue{{ID: "value-1", Name: "shipment"}}}, {ID: "axis-2", Name: "Ignored", Values: []msg.ClassificationValue{{ID: "value-2", Name: "hidden"}}}}}
	digest, _ := taxonomy.SemanticDigest()
	return &decisionStore{settings: msg.BoardClassification{BoardID: "board", OrganizationID: "org", Taxonomy: &taxonomy, TaxonomyRevision: 7, TaxonomyDigest: digest, Policy: &msg.BoardClassificationPolicy{Revision: 3, AxisPolicies: map[string]msg.ClassificationAxisPolicy{"axis-1": {Mode: msg.ClassificationAxisModeAssisted}}}, CallerActions: []msg.BoardClassificationAction{msg.BoardClassificationActionClassify}}, source: json.RawMessage(`{"board":{"id":"board"},"cards":[{"item":{"id":"card","title":"Shipment Ω","body":"Full shipment text\nwith untouched whitespace  ","updated_at":"2026-09-29T00:00:00Z"}}]}`)}
}
func runBoard(t *testing.T, s *decisionStore, organization string) (msg.OperationReceipt, *executorstest.FakeOneShot) {
	t.Helper()
	db, err := operationstore.Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	caller := &executorstest.FakeOneShot{InstanceID: "fake", DefaultModel: "test-model"}
	coordinator, err := operations.NewCoordinator(db, operations.Config{WorkerCount: 1, LeaseDuration: 3 * time.Second, LeaseOwner: "test"}, executors.BoardClassifier{Caller: caller, Store: s})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(msg.BoardClassificationRunInput{BoardID: "board", CardID: "card"})
	receipt, _, err := coordinator.Submit(msg.OperationIntent{Type: msg.OperationTypeBoardClassificationRun, OrganizationID: organization, PrincipalID: "person", IdempotencyKey: "test", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	s.beforePublish = func() {
		stored, e := db.Get(receipt.ID)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, e := range stored.Receipt.Evidence {
			if e.Kind == "classification_checkpoint" {
				found = true
			}
		}
		if !found {
			t.Fatal("model proposal was not durable before publication")
		}
	}
	if _, err := coordinator.RunNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := db.Get(receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record.Receipt, caller
}
func TestBoardClassificationPublishesStoredSourceAndStableSelections(t *testing.T) {
	s := boardFixture()
	r, c := runBoard(t, s, "org")
	if r.State != msg.OperationStateSucceeded {
		t.Fatalf("%+v", r.Error)
	}
	if len(s.publications) != 1 || len(c.Calls()) != 1 {
		t.Fatal("not exactly one model call and publication")
	}
	p := s.publications[0]
	if p.Source.Body != "Full shipment text\nwith untouched whitespace  " || p.Source.Subject != "Shipment Ω" || p.TaxonomyRevision != 7 || p.PolicyRevision != 3 {
		t.Fatalf("lost source/version: %+v", p)
	}
	if len(p.Selections) != 1 || p.Selections["axis-1"][0] != "value-1" {
		t.Fatalf("wrong stable selection: %+v", p.Selections)
	}
	if strings.Contains(c.Calls()[0].SystemPrompt, "Ignored") {
		t.Fatal("off axis sent to model")
	}
	if len(r.Effects) != 1 || r.Effects[0].Outcome != msg.OperationEffectOutcomeApplied {
		t.Fatal("publication not accounted for")
	}
}
func TestBoardClassificationRefusesBeforeModelCall(t *testing.T) {
	for _, test := range []string{"organization", "permission", "policy", "card"} {
		t.Run(test, func(t *testing.T) {
			s := boardFixture()
			org := "org"
			switch test {
			case "organization":
				org = "other"
			case "permission":
				s.settings.CallerActions = nil
			case "policy":
				s.settings.Policy = nil
			case "card":
				s.source = json.RawMessage(`{"board":{"id":"board"},"cards":[]}`)
			}
			r, c := runBoard(t, s, org)
			if r.State != msg.OperationStateFailed || len(c.Calls()) != 0 || len(s.publications) != 0 {
				t.Fatalf("refusal called model: %+v", r)
			}
		})
	}
}
func TestPublicationFailureIsUnknownAndNotAutomaticallyReplayed(t *testing.T) {
	s := boardFixture()
	s.publishErr = errors.New("connection lost")
	r, _ := runBoard(t, s, "org")
	if r.State != msg.OperationStateUnknown || r.Effects[0].Outcome != msg.OperationEffectOutcomeUnknown {
		t.Fatalf("%+v", r)
	}
	result := (executors.BoardClassifier{}).Reconcile(context.Background(), r)
	if result.Action != operations.ReconcileSettle || result.State != msg.OperationStateUnknown {
		t.Fatal("unsafe rerun")
	}
}
