package main

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/kanbanclient"
)

func TestASessionHostComesFromTheWorkBoardAndNeedsAnInstance(t *testing.T) {
	host, err := sessionHostFromBoard(&kanbanclient.Board{
		ID: "board-1", Name: "Northwind work", DefaultInstanceID: "inst-cc-local", DefaultBundleID: "6",
	})
	if err != nil {
		t.Fatalf("a board with an instance: %v", err)
	}
	if host.InstanceID != "inst-cc-local" || host.BundleID != "6" {
		t.Errorf("host = %+v, want the board's instance and bundle verbatim", host)
	}

	host, err = sessionHostFromBoard(&kanbanclient.Board{ID: "board-1", Name: "Northwind work", DefaultInstanceID: "inst-cc-local"})
	if err != nil || host.BundleID != "" {
		t.Errorf("a board with no bundle must spawn with none, got %+v %v", host, err)
	}

	_, err = sessionHostFromBoard(&kanbanclient.Board{ID: "board-1", Name: "Northwind work"})
	if err == nil {
		t.Fatal("a board with no default_instance_id spawned somewhere; there is no fallback instance")
	}
	if !strings.Contains(err.Error(), "board-1") || !strings.Contains(err.Error(), "default_instance_id") {
		t.Errorf("the refusal must name the board and the setting to fix: %v", err)
	}
}
