package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestStopReportsRefusalAndTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantErrSub string
	}{
		{"ok", http.StatusOK, ""},
		{"no content", http.StatusNoContent, ""},
		{"unknown session", http.StatusNotFound, "404"},
		{"forbidden", http.StatusForbidden, "403"},
		{"server error", http.StatusInternalServerError, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/sessions/br_1/stop" {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("session not found"))
			}))
			defer srv.Close()
			d := &delegate{client: srv.Client(), server: srv.URL}
			err := d.requestStop("br_1")
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("requestStop: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("requestStop error = %v, want one naming %s", err, tc.wantErrSub)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		d := &delegate{client: http.DefaultClient, server: url}
		if err := d.requestStop("br_1"); err == nil {
			t.Fatal("requestStop to a closed server returned nil")
		}
	})
}
