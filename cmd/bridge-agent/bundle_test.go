package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadBundleSendsTheConfigItselfNotAPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "mcp")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "none.json"), []byte(`{"mcpServers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"servers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	config, err := loadBundle("none")
	if err != nil {
		t.Fatal(err)
	}
	if config["mcp_config"] != `{"mcpServers":{}}` || config["strict_mcp_config"] != true {
		t.Errorf("config = %v; want the bundle's JSON, strict", config)
	}
	for _, name := range []string{"broken", "missing"} {
		if _, err := loadBundle(name); err == nil {
			t.Errorf("loadBundle(%q) succeeded", name)
		}
	}
}
