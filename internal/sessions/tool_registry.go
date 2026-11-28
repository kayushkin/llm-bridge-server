package sessions

import (
	"context"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/agents/tools"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// ToolRegistry is what the server asks the tool registry while it creates and
// spawns sessions: which harness tools exist and which are on, which tools an
// instance opted in to, the MCP config of a set of tools, and the tools a
// harness reported. The tool-store module answers it in this process
// (InProcessToolRegistry); a nil registry is a server whose tool-store module
// is off because it has no Postgres.
type ToolRegistry interface {
	ListTools(ctx context.Context, filter tools.ListFilter) ([]tools.Tool, error)
	ListInstanceTools(ctx context.Context, instanceID string) ([]tools.Tool, error)
	Provision(ctx context.Context, request tools.ProvisionRequest) (*tools.ProvisionResponse, error)
	RecordObservedHarnessTools(ctx context.Context, request tools.ObservedHarnessToolsRequest) (tools.ObservedHarnessToolsResponse, error)
}

// toolStoreOffMessage says why a server has no tool registry.
const toolStoreOffMessage = "this server's tool-store module is off (" + config.PostgresURLEnvironmentVariable + " is not set)"

// InProcessToolRegistry is the tool-store module, called directly.
type InProcessToolRegistry struct {
	Store             *tools.Store
	ResolveCredential tools.ResolveCredentialFunc
}

func (registry InProcessToolRegistry) ListTools(ctx context.Context, filter tools.ListFilter) ([]tools.Tool, error) {
	return registry.Store.ListTools(ctx, filter)
}

func (registry InProcessToolRegistry) ListInstanceTools(ctx context.Context, instanceID string) ([]tools.Tool, error) {
	return registry.Store.ListInstanceTools(ctx, instanceID)
}

func (registry InProcessToolRegistry) Provision(ctx context.Context, request tools.ProvisionRequest) (*tools.ProvisionResponse, error) {
	return tools.Provision(ctx, registry.Store, request, registry.ResolveCredential)
}

func (registry InProcessToolRegistry) RecordObservedHarnessTools(ctx context.Context, request tools.ObservedHarnessToolsRequest) (tools.ObservedHarnessToolsResponse, error) {
	return registry.Store.RecordObservedHarnessTools(ctx, request)
}

// CredentialResolverFromAuthStore resolves the auth-store provider an MCP
// tool's environment key names to the secret provisioning puts in its config,
// the way the standalone tool-store's resolver did: an API key for api_key, an
// access token for oauth and token, and an error for anything else or an empty
// secret. Nothing falls back to this process's environment.
func CredentialResolverFromAuthStore(client *authstoreclient.Client) tools.ResolveCredentialFunc {
	return func(ctx context.Context, provider string) (string, error) {
		if provider == "" {
			return "", fmt.Errorf("auth-store: empty provider")
		}
		resolved, err := client.ResolveByProvider(ctx, provider, "", "", "provision:"+provider)
		if err != nil {
			return "", err
		}
		switch resolved.AuthType {
		case "api_key":
			if resolved.APIKey == "" {
				return "", fmt.Errorf("auth-store: provider %q has empty api_key", provider)
			}
			return resolved.APIKey, nil
		case "oauth", "token":
			if resolved.AccessToken == "" {
				return "", fmt.Errorf("auth-store: provider %q has empty access_token", provider)
			}
			return resolved.AccessToken, nil
		default:
			return "", fmt.Errorf("auth-store: provider %q has unsupported auth_type %q", provider, resolved.AuthType)
		}
	}
}
