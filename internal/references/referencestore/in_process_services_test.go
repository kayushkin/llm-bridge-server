package referencestore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A type of a service in this process is read by its reader, for nobody
// when its access is the grant's and as the caller when the owner decides.
func TestAnInProcessTypeIsReadByItsReader(t *testing.T) {
	predictionsRead := 0
	services := InProcessServices{
		Services: []string{"prediction-store", "llm-bridge-server"},
		Readers: map[string]InProcessReader{
			"prediction": {
				Record: func(_ context.Context, id string, asker InProcessAsker) (json.RawMessage, error) {
					predictionsRead++
					if asker.PrincipalID != "" {
						t.Errorf("a grant-decided prediction was read as %s", asker.PrincipalID)
					}
					if id != "prediction_000001" {
						return nil, ErrRecordNotFound
					}
					return json.RawMessage(`{"id":"prediction_000001","claim":"it rains"}`), nil
				},
				Search: func(context.Context, string, InProcessAsker) (json.RawMessage, error) {
					return json.RawMessage(`[]`), nil
				},
			},
			"session": {
				Record: func(_ context.Context, id string, asker InProcessAsker) (json.RawMessage, error) {
					switch asker.PrincipalID {
					case "principal_000002":
						return json.RawMessage(`{"session_id":"` + id + `","display_name":"a chat"}`), nil
					case "principal_000004":
						return nil, ErrRefusedByOwner
					}
					return nil, ErrRecordNotFound
				},
			},
		},
	}
	resolver := &Resolver{
		Types:       Types,
		InProcess:   services,
		Credentials: &Credentials{},
		Access:      &AccessChecker{Directory: fakeAccess{}, TTL: time.Minute},
		RecordTTL:   time.Minute, MissTTL: time.Minute, LookupTimeout: time.Second, Concurrency: 2,
	}
	results, errs := resolver.Resolve(context.Background(), Caller{PrincipalID: "principal_000002"},
		[]string{"prediction_000001", "prediction_000002", "br_1794083694355010889"})
	if len(errs) != 0 {
		t.Fatalf("errors: %+v", errs)
	}
	if m := results["prediction_000001"]; len(m) != 1 || m[0].Refused || m[0].Label != "it rains" {
		t.Errorf("the prediction: %+v", m)
	}
	if m := results["prediction_000002"]; len(m) != 0 {
		t.Errorf("a missing prediction: %+v", m)
	}
	if m := results["br_1794083694355010889"]; len(m) != 1 || m[0].Refused || m[0].Label != "a chat" {
		t.Errorf("the caller's session: %+v", m)
	}
	other, errs := resolver.Resolve(context.Background(), Caller{PrincipalID: "principal_000003"}, []string{"br_1794083694355010889"})
	if len(errs) != 0 || len(other["br_1794083694355010889"]) != 0 {
		t.Errorf("another principal's session reads as unknown: %+v %+v", other, errs)
	}
	refused, errs := resolver.Resolve(context.Background(), Caller{PrincipalID: "principal_000004"}, []string{"br_1794083694355010889"})
	if len(errs) != 0 || len(refused["br_1794083694355010889"]) != 1 || !refused["br_1794083694355010889"][0].Refused {
		t.Errorf("an owner's refusal reads as refused: %+v %+v", refused, errs)
	}
	if predictionsRead != 2 {
		t.Errorf("prediction-store was read %d times, want 2", predictionsRead)
	}
}

// A type of a service in this process with no reader is refused at start,
// and so is a reader for a type no row declares; such a type is never asked
// over the network.
func TestAnInProcessTypeWithNoReaderIsRefused(t *testing.T) {
	err := ValidateInProcessReaders(Types, InProcessServices{
		Services: []string{"prediction-store"},
		Readers:  map[string]InProcessReader{"no_such_type": {}},
	})
	if err == nil || !strings.Contains(err.Error(), `type "prediction" of prediction-store has no in-process record reader`) ||
		!strings.Contains(err.Error(), `type "prediction" of prediction-store has no in-process search`) ||
		!strings.Contains(err.Error(), `which no row declares`) {
		t.Fatalf("validation = %v", err)
	}
	resolver := &Resolver{Types: Types, InProcess: InProcessServices{Services: []string{"prediction-store"}}, Credentials: &Credentials{},
		Access: &AccessChecker{Directory: fakeAccess{}, TTL: time.Minute}, LookupTimeout: time.Second, Concurrency: 1,
		ServiceURLs: map[string]string{"prediction-store": "http://127.0.0.1:1"}}
	_, errs := resolver.Resolve(context.Background(), Caller{Unchecked: true}, []string{"prediction_000001"})
	if len(errs) != 1 || !strings.Contains(errs[0].Error, "has no reader") {
		t.Fatalf("an unread in-process type: %+v", errs)
	}
}
