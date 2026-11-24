package referencestore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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

// Every service a type names has an address in the unit, and every page site
// one too, so the deployed service starts.
func TestTheUnitAddressesEveryService(t *testing.T) {
	unit, err := os.ReadFile("reference-store.service")
	if err != nil {
		t.Fatal(err)
	}
	read := func(variable string) map[string]string {
		match := regexp.MustCompile(`(?m)^Environment=` + variable + `=(.*)$`).FindSubmatch(unit)
		if match == nil {
			t.Fatalf("the unit sets no %s", variable)
		}
		pairs := map[string]string{}
		for _, pair := range strings.Split(string(match[1]), ",") {
			key, value, _ := strings.Cut(pair, ":")
			pairs[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		return pairs
	}
	serviceURLs, siteURLs := read("REFERENCE_STORE_SERVICE_URLS"), read("REFERENCE_STORE_SITE_URLS")
	for _, row := range Types {
		if (row.Get != "" || row.Search != "") && serviceURLs[row.Service] == "" {
			t.Errorf("unit has no address for %s", row.Service)
		}
		if row.PageSite != "" && siteURLs[row.PageSite] == "" {
			t.Errorf("unit has no address for site %s", row.PageSite)
		}
	}
	if err := ValidateReferenceConfiguration(Types, serviceURLs, siteURLs); err != nil {
		t.Fatal(err)
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
	mux.HandleFunc("GET /persons/{id}", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"` + r.PathValue("id") + `","is_administrator":` + map[bool]string{true: "true", false: "false"}[r.PathValue("id") == "principal_000001"] + `}`))
	})
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(PrincipalIDHeader) != r.PathValue("id") {
			http.Error(w, "a principal reads only its own", http.StatusForbidden)
			return
		}
		switch r.PathValue("id") {
		case "principal_000002":
			w.Write([]byte(`[{"relation":"can_see_references","resource_type":"reference_type","resource_id":"prediction"},
				{"relation":"can_see_references","resource_type":"reference","resource_id":"person_000007"},
				{"relation":"can_see_references","resource_type":"reference_type","resource_id":"person","revoked_at":5}]`))
		default:
			w.Write([]byte(`[]`))
		}
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func newTestServer(t *testing.T) (*httptest.Server, *stores) {
	s := newStores(t)
	urls := map[string]string{"prediction-store": s.server.URL, "people-store": s.server.URL, "llm-bridge-server": s.server.URL}
	resolver := &Resolver{
		Types: Types, ServiceURLs: urls,
		Credentials: &Credentials{BridgeServiceToken: "bridge-token", HTTPClient: http.DefaultClient},
		Access:      &AccessChecker{PrincipalStoreURL: s.server.URL, GrantStoreURL: s.server.URL, HTTPClient: http.DefaultClient, TTL: time.Minute},
		HTTPClient:  http.DefaultClient, RecordTTL: time.Minute, MissTTL: time.Minute, LookupTimeout: 5 * time.Second, Concurrency: 4,
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Server{Resolver: resolver, ServiceToken: "service-token"})
	server := httptest.NewServer(mux)
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
	resolveAs(t, server, ServiceTokenHeader, "service-token", "prediction_000001", "prediction_000009")
	results := resolveAs(t, server, ServiceTokenHeader, "service-token", "prediction_000001", "prediction_000009")
	if got := stores.fetches.Load(); got != 2 {
		t.Errorf("prediction-store was asked %d times, want 2", got)
	}
	if len(results["prediction_000009"]) != 0 {
		t.Errorf("a miss: %+v", results["prediction_000009"])
	}
}

func TestACallerMustBeNamed(t *testing.T) {
	server, _ := newTestServer(t)
	for _, header := range []http.Header{{}, {ServiceTokenHeader: {"wrong"}}} {
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
	s := newStores(t)
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
		Access:      &AccessChecker{PrincipalStoreURL: s.server.URL, GrantStoreURL: s.server.URL, HTTPClient: http.DefaultClient, TTL: time.Minute},
		HTTPClient:  http.DefaultClient, RecordTTL: time.Minute, MissTTL: time.Minute, LookupTimeout: 5 * time.Second, Concurrency: 4,
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Server{Resolver: resolver, ServiceToken: "service-token"})
	server := httptest.NewServer(mux)
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
