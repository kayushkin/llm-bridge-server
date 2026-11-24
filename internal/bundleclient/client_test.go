package bundleclient

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/bundlestore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

func newBundleStore(t *testing.T) *bundlestore.Store {
	t.Helper()
	module, err := bundlestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	return bundlestore.New(postgrestest.NewDatabase(t, module).Open(module))
}

func upsert(t *testing.T, store *bundlestore.Store, bundle *bundlestore.Bundle) int64 {
	t.Helper()
	if _, err := store.UpsertBundle(context.Background(), bundle); err != nil {
		t.Fatalf("upsert %s: %v", bundle.Name, err)
	}
	stored, err := store.GetBundleByName(context.Background(), bundle.Name)
	if err != nil {
		t.Fatal(err)
	}
	return stored.ID
}

// The client answers from the module in this process: a bundle resolves with
// base, its tools, skills and denials; a disabled or missing one, or a name,
// is ErrNotFound; and a disabled bundle still exists.
func TestTheClientAnswersFromTheModule(t *testing.T) {
	store := newBundleStore(t)
	ctx := context.Background()
	upsert(t, store, &bundlestore.Bundle{Name: "base", Enabled: true, Members: []bundlestore.Member{{Kind: bundlestore.MemberSkill, ID: 40, Name: "a-skill"}}})
	docker := upsert(t, store, &bundlestore.Bundle{Name: "docker", Enabled: true, DeniedReadPaths: []string{"/proc/**"}, Model: "a-model",
		Members: []bundlestore.Member{{Kind: bundlestore.MemberTool, ID: 13, Name: "a-tool"}, {Kind: bundlestore.MemberTool, ID: 102, Name: "a-shell", Effect: bundlestore.MemberDeny}}})
	off := upsert(t, store, &bundlestore.Bundle{Name: "off", Enabled: false})
	client := New(store)

	resolution, err := client.Resolve(ctx, strconv.FormatInt(docker, 10))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ids := resolution.ToolIDs(); len(ids) != 1 || ids[0] != 13 {
		t.Errorf("tools = %v, want [13]", ids)
	}
	if ids := resolution.DeniedToolIDs(); len(ids) != 1 || ids[0] != 102 {
		t.Errorf("denied tools = %v, want [102]", ids)
	}
	if len(resolution.Skills) != 1 || resolution.Skills[0].ID != 40 || len(resolution.Bundles) != 2 {
		t.Errorf("skills %v and bundles %v, want base's skill 40 and two bundles", resolution.Skills, resolution.Bundles)
	}
	if len(resolution.DeniedReadPaths) != 1 || resolution.Model != "a-model" {
		t.Errorf("denied read paths %v, model %q", resolution.DeniedReadPaths, resolution.Model)
	}

	if err := client.CheckExists(ctx, strconv.FormatInt(off, 10)); err != nil {
		t.Errorf("a disabled bundle exists: %v", err)
	}
	for _, id := range []string{strconv.FormatInt(off, 10), "999", "docker"} {
		if _, err := client.Resolve(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("resolve %s: err = %v, want ErrNotFound", id, err)
		}
	}
	if err := client.CheckExists(ctx, "999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("check 999: err = %v, want ErrNotFound", err)
	}
}
