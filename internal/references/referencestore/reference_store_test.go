package referencestore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// No two resolvable types claim the same id: every pattern is tried against
// an id made from every other type's prefix.
func TestNoTwoTypesClaimOneID(t *testing.T) {
	samples := map[string]string{
		"session": "br_1794083694355010889", "note": "84d66434-6666-4d0f-9c0e-c009e090f7f4",
		"repo": "repo:dash", "commit": "dash@e5cbe2e",
	}
	for _, row := range Types {
		if !row.Resolvable() {
			continue
		}
		sample, ok := samples[row.Type]
		if !ok {
			prefix := strings.TrimSuffix(row.IDPatterns[0], `_\d{6,}`)
			if prefix == row.IDPatterns[0] {
				t.Fatalf("type %s: no sample id for pattern %q; add one to samples", row.Type, row.IDPatterns[0])
			}
			sample = prefix + "_000001"
		}
		var claimants []string
		for i := range Types {
			if Types[i].Resolvable() && Types[i].Matches(sample) {
				claimants = append(claimants, Types[i].Type)
			}
		}
		if len(claimants) != 1 || claimants[0] != row.Type {
			t.Errorf("%s is claimed by %v, want only %s", sample, claimants, row.Type)
		}
	}
}

func TestPatternsMatchTheWholeID(t *testing.T) {
	row := TypeNamed("prediction")
	for _, id := range []string{"xprediction_000001", "prediction_000001x", "prediction_00001"} {
		if row.Matches(id) {
			t.Errorf("prediction matches %q", id)
		}
	}
}

func TestPageURLFillsTheIDAndRecordFields(t *testing.T) {
	r := &Resolver{SiteURLs: map[string]string{"music": "https://music.example"}}
	data := json.RawMessage(`{"id":"possession_000036","repo":{"id":15},"commit":{"sha":"abc1234"},"messages":[{"room_id":"!a:b"}]}`)
	cases := []struct{ typeName, id, want string }{
		{"garment", "garment_000001", "/possessions?id=possession_000036"},
		{"commit", "dash@abc1234", "/?view=work-graph&repo=15&commit=abc1234"},
		{"message", "message_000001", "/messages?room=%21a%3Ab"},
		{"music_song", "music_song_000001", "https://music.example/music/records/music_song_000001"},
		// A field the record lacks: no link rather than a wrong one.
		{"conversation_day", "conversation_day_000001", ""},
	}
	for _, c := range cases {
		if got := r.pageURL(TypeNamed(c.typeName), c.id, data); got != c.want {
			t.Errorf("%s: page %q, want %q", c.typeName, got, c.want)
		}
	}
}

// stores fakes the owning stores, principal-store and grant-store.
type stores struct {
	server  *httptest.Server
	fetches atomic.Int32
}

func newStores(t *testing.T) *stores {
	s := &stores{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /predictions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.fetches.Add(1)
		if r.PathValue("id") != "prediction_000001" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":"prediction_000001","claim":"it rains"}`))
	})
	// people-store keeps an owner on every person and shows a person only to
	// its owner and an administrator: person_000007 and person_000008 are
	// principal_000002's, person_000009 is principal_000003's, and the rest
	// principal_000001's.
	personOwners := map[string]string{"person_000007": "principal_000002", "person_000008": "principal_000002", "person_000009": "principal_000003"}
	mux.HandleFunc("GET /persons/{id}", func(w http.ResponseWriter, r *http.Request) {
		owner := personOwners[r.PathValue("id")]
		if owner == "" {
			owner = "principal_000001"
		}
		if caller := r.Header.Get(PrincipalIDHeader); caller != "" && caller != "principal_000001" && caller != owner {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":"` + r.PathValue("id") + `","display_name":"Someone"}`))
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-LLM-Bridge-Service-Token") != "bridge-token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		if r.Header.Get(PrincipalIDHeader) != "principal_000001" {
			http.Error(w, "not yours", http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"session_id":"` + r.PathValue("id") + `","display_name":"a chat"}`))
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// fakeAccess is principal-store and grant-store: principal_000001 is an
// administrator, principal_000002 holds a type grant on prediction, a record
// grant on person_000007 and on person_000009 (which another principal owns)
// and a revoked type grant on person, and every other
// principal holds nothing.
type fakeAccess struct{}

func (fakeAccess) Standing(_ context.Context, principalID string) (PrincipalStanding, error) {
	return PrincipalStanding{IsAdministrator: principalID == "principal_000001"}, nil
}

func (fakeAccess) Grants(_ context.Context, principalID string) ([]HeldGrant, error) {
	if principalID != "principal_000002" {
		return nil, nil
	}
	return []HeldGrant{
		{Relation: RelationCanSeeReferences, ResourceType: ResourceTypeReferenceType, ResourceID: "prediction"},
		{Relation: RelationCanSeeReferences, ResourceType: ResourceTypeReference, ResourceID: "person_000007"},
		{Relation: RelationCanSeeReferences, ResourceType: ResourceTypeReference, ResourceID: "person_000009"},
		{Relation: RelationCanSeeReferences, ResourceType: ResourceTypeReferenceType, ResourceID: "person", Revoked: true},
	}, nil
}

// uncheckedHeader stands in, in these tests, for the internal service: the
// server this store is mounted in sets the caller from the credential it
// checked, and testCallers sets it from headers.
const uncheckedHeader = "X-Test-Unchecked"

// testCallers serves mux with the caller named by the request's headers.
func testCallers(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if r.Header.Get(uncheckedHeader) == "yes" {
			ctx = ContextWithCaller(ctx, Caller{Unchecked: true})
		} else if principalID := r.Header.Get(PrincipalIDHeader); principalID != "" {
			ctx = ContextWithCaller(ctx, Caller{PrincipalID: principalID})
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newTestServer(t *testing.T) (*httptest.Server, *stores) {
	s := newStores(t)
	urls := map[string]string{"prediction-store": s.server.URL, "people-store": s.server.URL, "llm-bridge-server": s.server.URL}
	resolver := &Resolver{
		Types: Types, ServiceURLs: urls,
		Credentials: &Credentials{BridgeServiceToken: "bridge-token", HTTPClient: http.DefaultClient},
		Access:      &AccessChecker{Directory: fakeAccess{}, TTL: time.Minute},
		HTTPClient:  http.DefaultClient, RecordTTL: time.Minute, MissTTL: time.Minute, LookupTimeout: 5 * time.Second, Concurrency: 4,
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Server{Resolver: resolver})
	server := httptest.NewServer(testCallers(mux))
	t.Cleanup(server.Close)
	return server, s
}

func resolveAs(t *testing.T, server *httptest.Server, header, value string, ids ...string) map[string][]Match {
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/resolve", bytes.NewReader(body))
	if header != "" {
		request.Header.Set(header, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /resolve answered %d", response.StatusCode)
	}
	var answer struct {
		Results map[string][]Match `json:"results"`
		Errors  []LookupError      `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Errors) > 0 {
		t.Fatalf("errors: %+v", answer.Errors)
	}
	return answer.Results
}

func TestAnAdministratorSeesEverything(t *testing.T) {
	server, _ := newTestServer(t)
	results := resolveAs(t, server, PrincipalIDHeader, "principal_000001", "prediction_000001", "person_000003", "br_1794083694355010889")
	for id, matches := range results {
		if len(matches) != 1 || matches[0].Refused || matches[0].Label == "" {
			t.Errorf("%s: %+v", id, matches)
		}
	}
	if got := results["prediction_000001"][0].PageURL; got != "/predictions?id=prediction_000001" {
		t.Errorf("page %q", got)
	}
}

func TestGrantsOpenATypeOrOneRecordAndNothingElse(t *testing.T) {
	server, _ := newTestServer(t)
	results := resolveAs(t, server, PrincipalIDHeader, "principal_000002", "prediction_000001", "person_000007", "person_000008")
	if m := results["prediction_000001"]; len(m) != 1 || m[0].Refused || m[0].Label != "it rains" {
		t.Errorf("the type grant: %+v", m)
	}
	if m := results["person_000007"]; len(m) != 1 || m[0].Refused {
		t.Errorf("the record grant: %+v", m)
	}
	// The type grant on person is revoked, so another person stays shut.
	if m := results["person_000008"]; len(m) != 1 || !m[0].Refused || m[0].Data != nil || m[0].Label != "" {
		t.Errorf("an ungranted record: %+v", m)
	}
}

// A person is decided by both: the grant, and people-store asked as the
// caller, which shows a person only to its owner. A record grant on someone
// else's person shows nothing.
func TestAGrantShowsNoPersonTheCallerDoesNotOwn(t *testing.T) {
	server, _ := newTestServer(t)
	results := resolveAs(t, server, PrincipalIDHeader, "principal_000002", "person_000007", "person_000009")
	if m := results["person_000007"]; len(m) != 1 || m[0].Refused {
		t.Errorf("an owned and granted person: %+v", m)
	}
	if m := results["person_000009"]; len(m) != 0 {
		t.Errorf("a granted person another principal owns: %+v", m)
	}
	if m := resolveAs(t, server, PrincipalIDHeader, "principal_000001", "person_000009")["person_000009"]; len(m) != 1 || m[0].Refused {
		t.Errorf("the administrator: %+v", m)
	}
}

func TestTheOwnerDecidesASessionAsTheCaller(t *testing.T) {
	server, _ := newTestServer(t)
	if m := resolveAs(t, server, PrincipalIDHeader, "principal_000003", "br_1794083694355010889")["br_1794083694355010889"]; len(m) != 1 || !m[0].Refused || m[0].Data != nil {
		t.Errorf("another principal's session: %+v", m)
	}
	if m := resolveAs(t, server, PrincipalIDHeader, "principal_000001", "br_1794083694355010889")["br_1794083694355010889"]; len(m) != 1 || m[0].Refused {
		t.Errorf("the owner's session: %+v", m)
	}
}

func TestAnAnswerIsReusedAndAMissIsEmpty(t *testing.T) {
	server, stores := newTestServer(t)
	resolveAs(t, server, uncheckedHeader, "yes", "prediction_000001", "prediction_000009")
	results := resolveAs(t, server, uncheckedHeader, "yes", "prediction_000001", "prediction_000009")
	if got := stores.fetches.Load(); got != 2 {
		t.Errorf("prediction-store was asked %d times, want 2", got)
	}
	if len(results["prediction_000009"]) != 0 {
		t.Errorf("a miss: %+v", results["prediction_000009"])
	}
}

func TestACallerMustBeNamed(t *testing.T) {
	server, _ := newTestServer(t)
	for _, header := range []http.Header{{}, {uncheckedHeader: {"no"}}} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/resolve", strings.NewReader(`{"ids":["prediction_000001"]}`))
		request.Header = header
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("headers %v: %d, want 401", header, response.StatusCode)
		}
	}
}

func TestAPictureGoesOnlyToWhoMaySeeTheRecord(t *testing.T) {
	photos := http.NewServeMux()
	photos.HandleFunc("GET /photos/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"` + r.PathValue("id") + `","description":"a beach"}`))
	})
	photos.HandleFunc("GET /photos/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xff, 0xd8, 0xff})
	})
	photoStore := httptest.NewServer(photos)
	defer photoStore.Close()
	resolver := &Resolver{
		Types: Types, ServiceURLs: map[string]string{"photo-store": photoStore.URL},
		Credentials: &Credentials{HTTPClient: http.DefaultClient},
		Access:      &AccessChecker{Directory: fakeAccess{}, TTL: time.Minute},
		HTTPClient:  http.DefaultClient, RecordTTL: time.Minute, MissTTL: time.Minute, LookupTimeout: 5 * time.Second, Concurrency: 4,
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Server{Resolver: resolver})
	server := httptest.NewServer(testCallers(mux))
	defer server.Close()
	get := func(principal string) (int, string, int) {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/attachments/photo_000001", nil)
		request.Header.Set(PrincipalIDHeader, principal)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, response.Header.Get("Content-Type"), len(body)
	}
	if status, contentType, size := get("principal_000001"); status != 200 || contentType != "image/jpeg" || size != 3 {
		t.Errorf("the administrator: %d %s %d", status, contentType, size)
	}
	if status, _, _ := get("principal_000003"); status != 404 {
		t.Errorf("someone who may not see it: %d, want 404", status)
	}
}
