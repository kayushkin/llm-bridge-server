package toolstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The tests in this file exist because Provision, resolveEnv and
// resolveRequestedToolNames each have several producers of "it failed", and
// every pre-existing test asserted only that an error came back. An assertion
// that a call returned its failure value cannot tell one producer from
// another, so a producer can stop working — or stop being reached — with the
// suite still green. Each test below names the wording its own producer
// writes, so deleting that producer reddens this test and nothing else covers
// for it.

// requireErrorNaming asserts that err is non-nil and carries want, and says
// which producer it was looking for when it is not.
func requireErrorNaming(t *testing.T, err error, producer, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want an error naming %q, got nil", producer, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: want an error naming %q, got %q", producer, want, err.Error())
	}
}

func TestProvisionNamesTheToolItCouldNotFind(t *testing.T) {
	s := openProvTest(t)
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"ghost"}}, nil)
	requireErrorNaming(t, err, "provision/tool-not-found", `tool "ghost" not found`)
}

// A lookup that fails for a reason other than "no such tool" must be reported
// as a lookup failure and must keep the underlying cause, so an operator can
// tell a typo apart from a broken database. A closed store is the seam: the
// driver answers with something that is not ErrNotFound.
func TestProvisionSeparatesABrokenLookupFromAMissingTool(t *testing.T) {
	s := openProvTest(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave"}}, nil)
	requireErrorNaming(t, err, "provision/lookup-failed", `lookup "brave"`)
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a broken lookup must not be reported as a missing tool: %v", err)
	}
}

// The by-name path refuses a non-MCP tool with its own wording. Without this,
// deleting the kind check is invisible: the tool is simply left out of the
// response and the pre-existing test still sees an error, because the
// fixture's CLI tool also trips a later guard.
func TestProvisionByNameNamesTheKindItRefused(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "ripgrep", Kind: KindCLI, CLI: &CLISpec{Command: "rg"}, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"ripgrep"}}, nil)
	requireErrorNaming(t, err, "provision/kind-must-be-mcp", `has kind "cli"`)
}

func TestProvisionNamesTheToolThatIsDisabled(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "x", Kind: KindMCP, MCP: &MCPSpec{Transport: "stdio", Command: "npx"}, Enabled: false,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"x"}}, nil)
	requireErrorNaming(t, err, "provision/tool-disabled", `tool "x" is disabled`)
}

// A stored transport nothing knows how to configure must fail loudly. It
// cannot be written through UpsertTool — validate refuses it — so the row is
// aged into that state directly, which is how a hand-edited or migrated
// database reaches this code. Deleting the guard leaves an entry with neither
// a command nor a URL in the config Claude Code is handed, and nothing else in
// the suite notices.
func TestProvisionRefusesAStoredTransportItCannotConfigure(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "odd", Kind: KindMCP, MCP: &MCPSpec{Transport: "stdio", Command: "npx"}, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE tools SET mcp_transport='grpc' WHERE name='odd'`); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"odd"}}, nil)
	requireErrorNaming(t, err, "provision/unknown-transport", `unknown mcp transport "grpc"`)
}

// A tool that needs credentials and a caller that supplied no resolver is the
// one case where provisioning could plausibly fall back to os.Getenv. The
// doc comment on resolveEnv says it must not, and this pins the refusal by its
// wording rather than by the fact that something failed.
func TestResolveEnvRefusesToProvisionWithNoResolver(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name:        "brave",
		Kind:        KindMCP,
		EnvKeys:     []string{"BRAVE_API_KEY"},
		Credentials: map[string]string{"BRAVE_API_KEY": "brave"},
		MCP:         &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled:     true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave"}}, nil)
	requireErrorNaming(t, err, "resolveEnv/no-resolver-configured", "no credential resolver is configured")
}

func TestResolveEnvNamesTheEnvKeyWithNoCredentialsMapping(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "brave", Kind: KindMCP, EnvKeys: []string{"BRAVE_API_KEY"},
		MCP: &MCPSpec{Transport: "stdio", Command: "npx"}, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave"}},
		func(context.Context, string) (string, error) { return "x", nil })
	requireErrorNaming(t, err, "resolveEnv/no-credentials-mapping", `env key "BRAVE_API_KEY" has no credentials mapping`)
}

// The resolver's own failure must reach the caller with the provider named and
// the cause kept, so "brave is not logged in" does not arrive as a bare
// "provision failed".
func TestResolveEnvKeepsTheResolversCauseAndNamesTheProvider(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "brave", Kind: KindMCP, EnvKeys: []string{"BRAVE_API_KEY"},
		Credentials: map[string]string{"BRAVE_API_KEY": "brave"},
		MCP:         &MCPSpec{Transport: "stdio", Command: "npx"}, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sentinel := errors.New("no enabled credential")
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave"}},
		func(context.Context, string) (string, error) { return "", sentinel })
	requireErrorNaming(t, err, "resolveEnv/resolver-failed", `(provider "brave")`)
	if !errors.Is(err, sentinel) {
		t.Fatalf("the resolver's cause must survive the wrap, got %v", err)
	}
}

func TestProvisionRefusesARequestNamingBothSources(t *testing.T) {
	s := openProvTest(t)
	_, err := Provision(context.Background(), s, ProvisionRequest{
		Tools: []string{"remote-mcp"}, InstanceID: "inst-1",
	}, nil)
	requireErrorNaming(t, err, "resolveNames/both-sources-rejected", "names more than one of tools, tool_ids and instance_id")
}

func TestProvisionRefusesARequestNamingNeitherSource(t *testing.T) {
	s := openProvTest(t)
	_, err := Provision(context.Background(), s, ProvisionRequest{}, nil)
	requireErrorNaming(t, err, "resolveNames/neither-source-rejected", "one of tools, tool_ids or instance_id is required")
}

// An instance whose opt-in list cannot be read must not look like an instance
// with no opt-ins. Both produce an empty config; only one of them is an error,
// and TestProvisionByInstanceWithNoOptInsIsEmptyNotAnError pins the other side
// of exactly this pair.
func TestProvisionSeparatesAnUnreadableOptInListFromAnEmptyOne(t *testing.T) {
	s := openProvTest(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := Provision(context.Background(), s, ProvisionRequest{InstanceID: "inst-1"}, nil)
	requireErrorNaming(t, err, "resolveNames/instance-list-error-propagates", `list tools for instance "inst-1"`)
}

// Provision's "has no mcp spec" branch is unreachable through the store, and
// this test pins the reason rather than the branch. scanTool builds an MCPSpec
// for every row whose kind is mcp, whatever the mcp columns hold, and
// Provision only reaches that branch after establishing the kind is mcp. So the
// guard cannot fire today; deleting it is invisible to any suite, which is why
// it is dismissed rather than pinned. If scanTool ever stops filling the spec,
// this test goes red and the guard becomes live — which is the moment somebody
// needs to know.
func TestAnMCPRowAlwaysCarriesASpecEvenWhenEveryColumnIsBlank(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "blank", Kind: KindMCP, MCP: &MCPSpec{Transport: "stdio", Command: "npx"}, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.DB().Exec(
		`UPDATE tools SET mcp_transport='', mcp_command='', mcp_args='', mcp_url='' WHERE name='blank'`,
	); err != nil {
		t.Fatalf("blank the mcp columns: %v", err)
	}
	tool, err := s.GetToolByName("blank")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if tool.MCP == nil {
		t.Fatal("scanTool left MCP nil for an mcp row; Provision's \"has no mcp spec\" guard is now reachable and needs a test of its own")
	}
}
