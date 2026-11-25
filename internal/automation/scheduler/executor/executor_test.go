package executor

import (
	"testing"
	"time"
)

func TestRunSuccess(t *testing.T) {
	r := Run("echo hello", 10*time.Second)
	if r.Status != "success" {
		t.Fatalf("expected success, got %s: %s", r.Status, r.Output)
	}
	if r.Output != "hello\n" {
		t.Fatalf("unexpected output: %q", r.Output)
	}
}

func TestRunError(t *testing.T) {
	r := Run("exit 1", 10*time.Second)
	if r.Status != "error" {
		t.Fatalf("expected error, got %s", r.Status)
	}
}
