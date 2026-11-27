package spendrefresh

import (
	"context"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

type fakeAuthStore struct {
	credentials []authstoreclient.Credential
	keys        map[string]string
	resolved    []string
}

func (f *fakeAuthStore) List(context.Context, authstoreclient.ListFilter) ([]authstoreclient.Credential, error) {
	return f.credentials, nil
}

func (f *fakeAuthStore) Resolve(_ context.Context, id, reason string) (*authstoreclient.Resolved, error) {
	f.resolved = append(f.resolved, id+":"+reason)
	key, known := f.keys[id]
	if !known {
		return nil, errors.New("no such credential")
	}
	return &authstoreclient.Resolved{ID: id, AuthType: "api_key", APIKey: key}, nil
}

func TestDiscoveryTakesTheFirstAnthropicKeyThatIsAnAdminKey(t *testing.T) {
	authStore := &fakeAuthStore{
		credentials: []authstoreclient.Credential{
			{ID: "cred_oauth", Provider: "anthropic", AuthType: "oauth"},
			{ID: "cred_plain", Provider: "anthropic", AuthType: "api_key"},
			{ID: "cred_openai", Provider: "openai", AuthType: "api_key"},
			{ID: "cred_admin", Provider: "anthropic", AuthType: "api_key"},
		},
		keys: map[string]string{"cred_plain": "sk-ant-api03-made-up", "cred_admin": "sk-ant-admin01-made-up", "cred_openai": "sk-admin-made-up"},
	}
	collector, err := DiscoverAnthropicCollector(context.Background(), authStore)
	if err != nil || collector == nil {
		t.Fatalf("want a collector, got %v, %v", collector, err)
	}
	key, err := collector.APIKeyFn()
	if err != nil || key != "sk-ant-admin01-made-up" {
		t.Fatalf("the collector must resolve the admin credential: %q, %v", key, err)
	}
	want := []string{"cred_plain:discover:admin-key", "cred_admin:discover:admin-key", "cred_admin:fetch:anthropic-spend-per-key"}
	if len(authStore.resolved) != len(want) {
		t.Fatalf("resolved %v, want %v", authStore.resolved, want)
	}
	for index := range want {
		if authStore.resolved[index] != want[index] {
			t.Fatalf("resolved %v, want %v", authStore.resolved, want)
		}
	}
}

func TestDiscoveryWithNoAdminKeyIsNoCollector(t *testing.T) {
	authStore := &fakeAuthStore{credentials: []authstoreclient.Credential{{ID: "cred_plain", Provider: "anthropic", AuthType: "api_key"}},
		keys: map[string]string{"cred_plain": "sk-ant-api03-made-up"}}
	collector, err := DiscoverAnthropicCollector(context.Background(), authStore)
	if err != nil || collector != nil {
		t.Fatalf("want no collector and no error, got %v, %v", collector, err)
	}
}
