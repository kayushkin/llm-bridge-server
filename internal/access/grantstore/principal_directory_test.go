package grantstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newPrincipalStoreServer answers the way principal-store does live: a
// principal with its active groups expanded, {"error":…} 404 for a missing
// one, and Go's plain 404 for a route it does not have.
func newPrincipalStoreServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/principals/principal_000001":
			w.Write([]byte(`{"id":"principal_000001","seq":1,"kind":"human","acts_in_this_deployment":true,"acts_as_caller":true,"display_name":"Slava Kayushkin","email":"","disabled_at":0,"groups":[{"id":"principal_000006","kind":"group"},{"id":"principal_000007","kind":"group"}]}`))
		case "/principals/principal_000006":
			w.Write([]byte(`{"id":"principal_000006","seq":6,"kind":"group","acts_in_this_deployment":true,"acts_as_caller":false,"display_name":"Data Team","disabled_at":0,"members":[{"id":"principal_000001"}]}`))
		case "/principals/principal_000005":
			w.Write([]byte(`{"id":"principal_000005","kind":"human","acts_in_this_deployment":true,"acts_as_caller":true,"display_name":"Gone","disabled_at":1789000000,"groups":[]}`))
		case "/principals/principal_000099":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found: principal principal_000099"}`))
		case "/principals/principal_000500":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"database is locked"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPDirectoryReadsAPrincipalAndItsGroups(t *testing.T) {
	srv := newPrincipalStoreServer(t)
	directory := NewHTTPPrincipalDirectory(srv.URL + "/")
	ctx := context.Background()

	slava, err := directory.LookupPrincipal(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if slava.Kind != "human" || strings.Join(slava.GroupIDs, ",") != "principal_000006,principal_000007" {
		t.Fatalf("slava = %+v", slava)
	}
	group, err := directory.LookupPrincipal(ctx, "principal_000006")
	if err != nil || group.Kind != "group" || len(group.GroupIDs) != 0 {
		t.Fatalf("group = %+v %v", group, err)
	}
	// Disabled still resolves — a grant to a departed human is dormant, not invalid.
	gone, err := directory.LookupPrincipal(ctx, "principal_000005")
	if err != nil || gone.DisabledAt == 0 {
		t.Fatalf("disabled = %+v %v", gone, err)
	}
}

func TestHTTPDirectoryTellsMissingFromBroken(t *testing.T) {
	srv := newPrincipalStoreServer(t)
	directory := NewHTTPPrincipalDirectory(srv.URL)
	ctx := context.Background()

	if _, err := directory.LookupPrincipal(ctx, "principal_000099"); !errors.Is(err, ErrPrincipalNotFound) {
		t.Fatalf("missing: err = %v, want ErrPrincipalNotFound", err)
	}
	_, err := directory.LookupPrincipal(ctx, "principal_000500")
	if err == nil || errors.Is(err, ErrPrincipalNotFound) || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("500: err = %v", err)
	}
	// A base URL pointing at a service without the route answers Go's plain
	// 404. That is a misconfiguration, not a missing principal.
	wrong := NewHTTPPrincipalDirectory(srv.URL + "/wrong-prefix")
	_, err = wrong.LookupPrincipal(ctx, "principal_000001")
	if err == nil || errors.Is(err, ErrPrincipalNotFound) || !strings.Contains(err.Error(), "PRINCIPAL_STORE_URL") {
		t.Fatalf("unrouted: err = %v, want a failure naming PRINCIPAL_STORE_URL", err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	_, err = NewHTTPPrincipalDirectory(goneURL).LookupPrincipal(ctx, "principal_000001")
	if err == nil || errors.Is(err, ErrPrincipalNotFound) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("unreachable: err = %v", err)
	}
}
