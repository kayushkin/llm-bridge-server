package aiauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth-profiles.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	cred := &Credential{
		Type:     "api_key",
		Provider: "anthropic",
		Key:      "sk-test-key-123",
	}
	if err := store.SetProfile("anthropic:default", cred); err != nil {
		t.Fatal(err)
	}

	// Re-read
	store2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	profiles := store2.Profiles()
	if profiles["anthropic:default"] == nil {
		t.Fatal("profile not found after reload")
	}
	if profiles["anthropic:default"].Key != "sk-test-key-123" {
		t.Fatal("key mismatch")
	}
}

func TestCredentialOrdering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth-profiles.json")

	data := &AuthStore{
		Version: 1,
		Profiles: map[string]*Credential{
			"anthropic:key": {Type: "api_key", Provider: "anthropic", Key: "api-key-1"},
			"anthropic:tok": {Type: "token", Provider: "anthropic", Token: "token-1"},
			"anthropic:oa":  {Type: "oauth", Provider: "anthropic", Access: "oauth-1", Expires: time.Now().Add(time.Hour).UnixMilli()},
		},
	}
	raw, _ := json.Marshal(data)
	os.WriteFile(path, raw, 0600)

	store, _ := NewStore(path)
	key, err := store.ResolveKey("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if key != "oauth-1" {
		t.Fatalf("expected oauth first, got %s", key)
	}
}

func TestEnvVarPriority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth-profiles.json")

	data := &AuthStore{
		Version: 1,
		Profiles: map[string]*Credential{
			"anthropic:key": {Type: "api_key", Provider: "anthropic", Key: "stored-key"},
		},
	}
	raw, _ := json.Marshal(data)
	os.WriteFile(path, raw, 0600)

	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	store, _ := NewStore(path)
	key, err := store.ResolveKey("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if key != "env-key" {
		t.Fatalf("expected env var, got %s", key)
	}
}

func TestExpiredTokenSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth-profiles.json")

	data := &AuthStore{
		Version: 1,
		Profiles: map[string]*Credential{
			"anthropic:tok": {Type: "token", Provider: "anthropic", Token: "expired-token", Expires: time.Now().Add(-time.Hour).UnixMilli()},
			"anthropic:key": {Type: "api_key", Provider: "anthropic", Key: "fallback-key"},
		},
	}
	raw, _ := json.Marshal(data)
	os.WriteFile(path, raw, 0600)

	store, _ := NewStore(path)
	key, err := store.ResolveKey("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if key != "fallback-key" {
		t.Fatalf("expected fallback to api_key, got %s", key)
	}
}

func TestPKCEGeneration(t *testing.T) {
	v, c, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 || len(c) == 0 {
		t.Fatal("empty PKCE values")
	}
	if v == c {
		t.Fatal("verifier and challenge should differ")
	}
}

func TestMaskKey(t *testing.T) {
	masked := MaskKey("sk-ant-api03-abcdefghijklmnop")
	if masked != "sk-a...mnop" {
		t.Fatalf("unexpected mask: %s", masked)
	}
}
