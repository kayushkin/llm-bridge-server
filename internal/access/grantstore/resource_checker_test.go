package grantstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newOwnerServer stands in for skill-store and tool-store at once, answering the way each does live. Any path it does not know gets Go's
// unrouted 404, as a real ServeMux would.
func newOwnerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/skills/7", "/tools/3", "/tools/a%2Fslash":
			w.Write([]byte(`{"id":"exists"}`))
		case "/skills/8":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"skill not found"}`))
		case "/tools/4":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"tool not found"}`))
		case "/tools/boom":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCheckerAgainst(srv *httptest.Server) *HTTPResourceChecker {
	return NewHTTPResourceChecker(ResourceOwners{SkillStoreURL: srv.URL + "/", ToolStoreURL: srv.URL})
}

func TestHTTPCheckerReadsTheOwnersAnswer(t *testing.T) {
	srv := newOwnerServer(t)
	checker := newCheckerAgainst(srv)
	ctx := context.Background()

	for _, ref := range [][2]string{
		{ResourceTypeSkill, "7"},
		{ResourceTypeTool, "3"},
		{ResourceTypeTool, "a/slash"}, // path-escaped, not split into two segments
	} {
		if err := checker.CheckResourceExists(ctx, ref[0], ref[1]); err != nil {
			t.Fatalf("%s %s should exist: %v", ref[0], ref[1], err)
		}
	}
	for _, ref := range [][2]string{
		{ResourceTypeSkill, "8"},
		{ResourceTypeTool, "4"},
	} {
		if err := checker.CheckResourceExists(ctx, ref[0], ref[1]); !errors.Is(err, ErrResourceNotFound) {
			t.Fatalf("%s %s: err = %v, want ErrResourceNotFound", ref[0], ref[1], err)
		}
	}
}

func TestHTTPCheckerReportsAFailingOwnerAsAFailureNotAMissingResource(t *testing.T) {
	ctx := context.Background()

	srv := newOwnerServer(t)
	checker := newCheckerAgainst(srv)
	err := checker.CheckResourceExists(ctx, ResourceTypeTool, "boom")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("500: err = %v, want a failure carrying the status and body", err)
	}

	// A base URL pointing at a service without the route answers Go's unrouted
	// 404. That is a misconfiguration, not a missing tool.
	unrouted := NewHTTPResourceChecker(ResourceOwners{ToolStoreURL: srv.URL + "/wrong-prefix"})
	err = unrouted.CheckResourceExists(ctx, ResourceTypeTool, "3")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "TOOL_STORE_URL") {
		t.Fatalf("unrouted 404: err = %v, want a failure naming TOOL_STORE_URL", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	err = NewHTTPResourceChecker(ResourceOwners{SkillStoreURL: goneURL, ToolStoreURL: goneURL}).CheckResourceExists(ctx, ResourceTypeSkill, "7")
	if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("unreachable: err = %v", err)
	}
}

func TestHTTPCheckerTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	checker := NewHTTPResourceChecker(ResourceOwners{SkillStoreURL: slow.URL, ToolStoreURL: slow.URL})
	if checker.client.Timeout != ownerCheckTimeout || ownerCheckTimeout != 3*time.Second {
		t.Fatalf("client timeout = %v, want 3s", checker.client.Timeout)
	}
	checker.client.Timeout = 50 * time.Millisecond
	err := checker.CheckResourceExists(context.Background(), ResourceTypeTool, "3")
	if err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("timeout: err = %v, want a failure", err)
	}
}

// TestTheHTTPCheckerAsksOnlyTheOwnersOutsideThisProcess: every resource type
// is either checked over HTTP or owned in the mounting server's own process,
// and asking this checker about one of the latter is an error naming why,
// never a "does not exist".
func TestTheHTTPCheckerAsksOnlyTheOwnersOutsideThisProcess(t *testing.T) {
	overHTTP := map[string]bool{}
	for _, resourceType := range ResourceTypesCheckedOverHTTP {
		overHTTP[resourceType] = true
		if _, err := lookupResourceType(resourceType); err != nil {
			t.Fatalf("%s is checked over HTTP but has no definition: %v", resourceType, err)
		}
	}
	checker := newCheckerAgainst(newOwnerServer(t))
	for _, resourceType := range ResourceTypes {
		if overHTTP[resourceType] {
			continue
		}
		err := checker.CheckResourceExists(context.Background(), resourceType, "1")
		if err == nil || errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "own process") {
			t.Fatalf("%s: want an error saying the mounting server checks it, got %v", resourceType, err)
		}
	}
}

func TestReferenceOwnerChecksAskReferenceStore(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /types/{type}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("type") != "prediction" {
			http.Error(w, `{"error":"no reference type"}`, http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"type":"prediction"}`))
	})
	mux.HandleFunc("POST /resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ReferenceStoreServiceTokenHeader) != "reference-token" {
			http.Error(w, "no caller", http.StatusUnauthorized)
			return
		}
		var body struct{ IDs []string }
		json.NewDecoder(r.Body).Decode(&body)
		switch body.IDs[0] {
		case "prediction_000012":
			w.Write([]byte(`{"results":{"prediction_000012":[{"type":"prediction"}]}}`))
		case "photo_000001":
			w.Write([]byte(`{"results":{"photo_000001":[]},"errors":[{"type":"photo","error":"photo-store answered 500"}]}`))
		default:
			w.Write([]byte(`{"results":{"` + body.IDs[0] + `":[]}}`))
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	checker := NewHTTPResourceChecker(ResourceOwners{ReferenceStoreURL: server.URL, ReferenceStoreServiceToken: "reference-token"})
	ctx := context.Background()
	if err := checker.CheckResourceExists(ctx, ResourceTypeReferenceType, "prediction"); err != nil {
		t.Errorf("a type that exists: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReferenceType, "nonsense"); !errors.Is(err, ErrResourceNotFound) {
		t.Errorf("a type that does not: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReference, "prediction_000012"); err != nil {
		t.Errorf("a record that exists: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReference, "prediction_000099"); !errors.Is(err, ErrResourceNotFound) {
		t.Errorf("a record that does not: %v", err)
	}
	if err := checker.CheckResourceExists(ctx, ResourceTypeReference, "photo_000001"); err == nil || errors.Is(err, ErrResourceNotFound) {
		t.Errorf("a store that could not say is a failure, not a miss: %v", err)
	}
}
