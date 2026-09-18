package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// injectMCPConfig gives a session the MCP servers it is entitled to, from
// either of two sources, and rewrites HarnessConfig so "mcp_config" holds the
// path of the generated config file (consumed by claudecode via --mcp-config;
// opaque pass-through for other harnesses).
//
// The two sources, in precedence order:
//
//  1. A "tool_store_tools" key on HarnessConfig — a JSON array of tool names
//     the caller asked for by hand. The key is removed afterwards: it is an
//     instruction, not state to ship. An empty array means "no MCP servers",
//     and is honoured as an explicit opt-out.
//  2. Otherwise, for a session started with a bundle (BundleID, normally a
//     kanban board's default_bundle_id), the tools bundle-store resolves the
//     bundle to — its extends chain plus base. A bundle is a per-session
//     request like source 1, so a bundle-store that cannot answer, a bundle
//     that resolves to tools tool-store cannot provision, or a bundle that
//     has gone missing since creation aborts the spawn. When the session is
//     also started as a principal holding can_use tool grants, only the
//     bundle's tools those grants name are offered (a bundle says what the
//     work needs; a grant says what the person may have); a principal with
//     no such grant is lenient exactly as in source 3. A bundle naming no
//     tools at all provisions nothing and does not fall through — the caller
//     chose a bundle, and its answer was "no MCP servers".
//  3. Otherwise, for a session started as a principal, the tools the
//     principal's effective can_use grants name (grant-store) that the
//     session's instance also has opted in (tool-store) — both must hold: a
//     grant says who may have it, an opt-in says where it is wired. Lenient by
//     the operator's choice on 2026-09-11: a principal holding no can_use tool
//     grant at all falls through to source 4 unchanged, with a log line saying
//     so. A grant-store that cannot answer aborts the spawn — a session that
//     named a principal must not be offered everything because the check
//     failed — where a tool-store that cannot answer starts it with nothing,
//     as source 4 does.
//  4. Otherwise, the opt-in list the session's instance carries in tool-store —
//     the rows the Tools page writes. This is what makes a tick on that page
//     reach a spawned session instead of sitting in a table nothing reads.
//
// The two sources fail differently, on purpose. A named list that cannot be
// provisioned aborts the spawn: the caller asked for those tools, and starting
// without them starts something other than what was asked for. An instance's
// opt-ins are a standing preference rather than a per-session request, so a
// tool-store that is down or a stale opt-in row logs loudly and the session
// starts with no MCP servers — the same state every session on this box is in
// today. A registry outage must not stop the fleet from working.
//
// If the caller already set "mcp_config" it wins outright and neither source is
// consulted.
//
// The mutation is in-memory only, matching injectHookSettings.
// Tool offer sources, in the precedence order the comment above describes.
const (
	toolOfferPreset    = "mcp_config"       // the caller set mcp_config outright
	toolOfferHandNamed = "tool_store_tools" // the caller named tools by hand
	toolOfferBundle    = "bundle"
	toolOfferGrants    = "grants"
	toolOfferInstance  = "instance"
	toolOfferNone      = "none"
)

// toolOffer is the MCP tools a session is offered and which source decided it.
// resolveToolOffer computes it; injectMCPConfig provisions it; the effective
// config view reports it. One decision, three readers.
type toolOffer struct {
	Source string
	// Names is the hand-named list (Source toolOfferHandNamed); IDs is every
	// other source's answer, in tool-store ids.
	Names []string
	IDs   []int64
	// Notes are what was consulted and how it narrowed or did not decide.
	Notes []string
	// Resolution is bundle-store's answer when Source is toolOfferBundle.
	Resolution *bundleclient.Resolution
	// NarrowedByGrants is true when a principal's can_use grants cut the
	// bundle's or the instance's list.
	NarrowedByGrants bool
}

// resolveToolOffer decides which MCP tools sess is offered, by the rules in
// the comment on injectMCPConfig, without provisioning anything. An error is
// a spawn that must not start; a lenient failure is a note on the offer.
func (s *Server) resolveToolOffer(ctx context.Context, sess *store.Session, cfg map[string]json.RawMessage) (*toolOffer, error) {
	raw, named := cfg["tool_store_tools"]
	_, preset := cfg["mcp_config"]
	if named && preset {
		return nil, fmt.Errorf("HarnessConfig has both mcp_config and tool_store_tools; pick one")
	}
	if preset {
		return &toolOffer{Source: toolOfferPreset, Notes: []string{"the caller set harness_config.mcp_config outright; no store was consulted"}}, nil
	}
	if named {
		var tools []string
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("tool_store_tools is not a string array: %w", err)
		}
		return &toolOffer{Source: toolOfferHandNamed, Names: tools}, nil
	}
	if sess.BundleID != "" {
		return s.resolveBundleToolOffer(ctx, sess)
	}
	if sess.InstanceID == "" {
		return &toolOffer{Source: toolOfferNone, Notes: []string{"the session has no instance, so there are no opt-ins to read"}}, nil
	}
	if sess.PrincipalID != "" {
		offer, err := s.resolveGrantedToolOffer(ctx, sess)
		if err != nil || offer != nil {
			return offer, err
		}
	}
	offer := &toolOffer{Source: toolOfferInstance}
	optedIn, err := s.instanceMCPTools(ctx, sess.InstanceID)
	if err != nil {
		offer.Notes = append(offer.Notes, fmt.Sprintf("instance %s's opt-ins could not be read from tool-store, so the session starts with no MCP servers: %v", sess.InstanceID, err))
		return offer, nil
	}
	for _, t := range optedIn {
		offer.IDs = append(offer.IDs, t.ID)
	}
	if len(offer.IDs) == 0 {
		offer.Notes = append(offer.Notes, fmt.Sprintf("instance %s has opted in to no MCP tools", sess.InstanceID))
	}
	return offer, nil
}

func (s *Server) resolveBundleToolOffer(ctx context.Context, sess *store.Session) (*toolOffer, error) {
	if s.bundleClient == nil {
		return nil, fmt.Errorf("session %s was started with bundle %s but this server has no bundle-store to resolve it with (LLMBRIDGE_BUNDLE_STORE_URL)", sess.SessionID, sess.BundleID)
	}
	resolution, err := s.bundleClient.Resolve(ctx, sess.BundleID)
	if err != nil {
		return nil, fmt.Errorf("session %s was started with bundle %s, which could not be resolved, so it was not started: %w", sess.SessionID, sess.BundleID, err)
	}
	offer := &toolOffer{Source: toolOfferBundle, Resolution: resolution, IDs: resolution.ToolIDs()}
	if sess.PrincipalID != "" {
		if s.grantClient == nil {
			return nil, fmt.Errorf("session %s is started as %s but this server has no grant-store to read its grants from (LLMBRIDGE_GRANT_STORE_URL)", sess.SessionID, sess.PrincipalID)
		}
		grantedIDs, err := s.grantClient.EffectiveToolIDs(ctx, sess.PrincipalID)
		if err != nil {
			return nil, fmt.Errorf("session %s is started as %s and its grants could not be read, so it was not started: %w", sess.SessionID, sess.PrincipalID, err)
		}
		if len(grantedIDs) == 0 {
			offer.Notes = append(offer.Notes, fmt.Sprintf("principal %s holds no can_use tool grant; the bundle's tools are offered unchanged (lenient, operator's choice 2026-09-11)", sess.PrincipalID))
		} else {
			granted := make(map[int64]bool, len(grantedIDs))
			for _, id := range grantedIDs {
				granted[id] = true
			}
			narrowed := make([]int64, 0, len(offer.IDs))
			for _, id := range offer.IDs {
				if granted[id] {
					narrowed = append(narrowed, id)
				}
			}
			if len(narrowed) < len(offer.IDs) {
				offer.NarrowedByGrants = true
				offer.Notes = append(offer.Notes, fmt.Sprintf("bundle %s names tools %v; %s's can_use grants allow %v; offering %v", sess.BundleID, offer.IDs, sess.PrincipalID, grantedIDs, narrowed))
			}
			offer.IDs = narrowed
		}
	}
	if len(resolution.Skills) > 0 {
		offer.Notes = append(offer.Notes, fmt.Sprintf("the bundle also names %d skills, which are not provisioned per session (Claude Code lists ~/.claude/skills wholesale)", len(resolution.Skills)))
	}
	return offer, nil
}

// resolveGrantedToolOffer is source 3: nil with no error exactly when the
// principal holds no can_use tool grant, so the caller falls through to the
// instance's opt-ins.
func (s *Server) resolveGrantedToolOffer(ctx context.Context, sess *store.Session) (*toolOffer, error) {
	if s.grantClient == nil {
		return nil, fmt.Errorf("session %s is started as %s but this server has no grant-store to read its grants from (LLMBRIDGE_GRANT_STORE_URL)", sess.SessionID, sess.PrincipalID)
	}
	grantedIDs, err := s.grantClient.EffectiveToolIDs(ctx, sess.PrincipalID)
	if err != nil {
		return nil, fmt.Errorf("session %s is started as %s and its grants could not be read, so it was not started: %w", sess.SessionID, sess.PrincipalID, err)
	}
	if len(grantedIDs) == 0 {
		return nil, nil
	}
	offer := &toolOffer{Source: toolOfferGrants}
	optedIn, err := s.instanceMCPTools(ctx, sess.InstanceID)
	if err != nil {
		offer.Notes = append(offer.Notes, fmt.Sprintf("instance %s's opt-ins could not be read from tool-store, so the session starts with no MCP servers: %v", sess.InstanceID, err))
		return offer, nil
	}
	granted := make(map[int64]bool, len(grantedIDs))
	for _, id := range grantedIDs {
		granted[id] = true
	}
	var optedInIDs []int64
	for _, t := range optedIn {
		optedInIDs = append(optedInIDs, t.ID)
		if granted[t.ID] {
			offer.IDs = append(offer.IDs, t.ID)
		}
	}
	offer.NarrowedByGrants = len(offer.IDs) < len(optedInIDs)
	if len(offer.IDs) == 0 {
		offer.Notes = append(offer.Notes, fmt.Sprintf("%s's granted tools %v and instance %s's opt-ins %v share nothing; the session starts with no MCP servers", sess.PrincipalID, grantedIDs, sess.InstanceID, optedInIDs))
	} else {
		offer.Notes = append(offer.Notes, fmt.Sprintf("granted %v ∩ instance %s opt-ins %v", grantedIDs, sess.InstanceID, optedInIDs))
	}
	return offer, nil
}

func (s *Server) injectMCPConfig(sess *store.Session) error {
	if sess == nil || sess.Harness != msg.HarnessClaudeCode {
		return nil
	}
	cfg := map[string]json.RawMessage{}
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			return fmt.Errorf("HarnessConfig unparseable: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	offer, err := s.resolveToolOffer(ctx, sess, cfg)
	if err != nil {
		return err
	}
	for _, note := range offer.Notes {
		log.Printf("[tool-offer] session %s: %s", sess.SessionID, note)
	}
	switch offer.Source {
	case toolOfferPreset, toolOfferNone:
		return nil
	case toolOfferHandNamed:
		delete(cfg, "tool_store_tools")
		if len(offer.Names) == 0 {
			return s.replaceHarnessConfig(sess, cfg)
		}
		path, err := s.writeProvisionedMCPConfig(map[string]any{"tools": offer.Names})
		if err != nil {
			return err
		}
		if path == "" {
			return fmt.Errorf("tool-store provisioned no servers for tools %v", offer.Names)
		}
		return s.setMCPConfigPath(sess, cfg, path, len(offer.Names))
	case toolOfferBundle:
		if len(offer.IDs) == 0 {
			log.Printf("[bundle-store] session %s bundle %s resolves to no MCP tools (bundles %v); starting with no MCP servers",
				sess.SessionID, sess.BundleID, offer.Resolution.Bundles)
			return s.replaceHarnessConfig(sess, cfg)
		}
		path, err := s.writeProvisionedMCPConfig(map[string]any{"tool_ids": offer.IDs})
		if err != nil {
			return fmt.Errorf("session %s bundle %s: provisioning tools %v failed, so it was not started: %w", sess.SessionID, sess.BundleID, offer.IDs, err)
		}
		if path == "" {
			return fmt.Errorf("session %s bundle %s: tool-store provisioned no servers for tools %v", sess.SessionID, sess.BundleID, offer.IDs)
		}
		if err := s.setMCPConfigPath(sess, cfg, path, len(offer.IDs)); err != nil {
			return err
		}
		log.Printf("[bundle-store] session %s: offered tools %v from bundle %s (bundles %v)", sess.SessionID, offer.IDs, sess.BundleID, offer.Resolution.Bundles)
		return nil
	case toolOfferGrants:
		if len(offer.IDs) == 0 {
			return nil
		}
		path, err := s.writeProvisionedMCPConfig(map[string]any{"tool_ids": offer.IDs})
		if err != nil {
			return fmt.Errorf("session %s as %s: provisioning granted tools %v failed, so it was not started: %w", sess.SessionID, sess.PrincipalID, offer.IDs, err)
		}
		if path == "" {
			return fmt.Errorf("session %s as %s: tool-store provisioned no servers for granted tools %v", sess.SessionID, sess.PrincipalID, offer.IDs)
		}
		if err := s.setMCPConfigPath(sess, cfg, path, len(offer.IDs)); err != nil {
			return err
		}
		log.Printf("[grant-store] session %s as %s: offered tools %v", sess.SessionID, sess.PrincipalID, offer.IDs)
		return nil
	case toolOfferInstance:
		// Provisioned by instance id, as before: tool-store applies the opt-in
		// list itself, and a registry outage starts the session with nothing
		// rather than stopping it.
		path, err := s.writeProvisionedMCPConfig(map[string]any{"instance_id": sess.InstanceID})
		if err != nil {
			log.Printf("[tool-store] instance %s opt-ins unavailable for session %s, starting with no MCP servers: %v",
				sess.InstanceID, sess.SessionID, err)
			return nil
		}
		if path == "" {
			return nil
		}
		return s.setMCPConfigPath(sess, cfg, path, 0)
	}
	return fmt.Errorf("tool offer source %q is not one this server knows how to provision", offer.Source)
}

// mcpTool is one row of tool-store's per-instance opt-in list that is an MCP
// server: the only kind a session can be handed.
type mcpTool struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Server) instanceMCPToolIDs(ctx context.Context, instanceID string) ([]int64, error) {
	tools, err := s.instanceMCPTools(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(tools))
	for _, t := range tools {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

func (s *Server) instanceMCPTools(ctx context.Context, instanceID string) ([]mcpTool, error) {
	requestURL := s.cfg.ToolStoreURL + "/instances/" + url.PathEscape(instanceID) + "/tools"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", requestURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tool-store response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tool-store GET %s returned %d: %s", requestURL, resp.StatusCode, string(body))
	}
	var tools []mcpTool
	if err := json.Unmarshal(body, &tools); err != nil {
		return nil, fmt.Errorf("tool-store GET %s returned unparseable JSON: %w", requestURL, err)
	}
	mcp := make([]mcpTool, 0, len(tools))
	for _, t := range tools {
		if t.Kind == "mcp" {
			mcp = append(mcp, t)
		}
	}
	return mcp, nil
}

// setMCPConfigPath points HarnessConfig's mcp_config at path and writes the
// result back onto the session. toolCount is only for the log line; 0 means
// the tools came from the instance's opt-in list rather than a named request.
func (s *Server) setMCPConfigPath(sess *store.Session, cfg map[string]json.RawMessage, path string, toolCount int) error {
	pathJSON, err := json.Marshal(path)
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("marshal mcp config path: %w", err)
	}
	cfg["mcp_config"] = pathJSON
	if err := s.replaceHarnessConfig(sess, cfg); err != nil {
		os.Remove(path)
		return err
	}
	if toolCount > 0 {
		log.Printf("[tool-store] provisioned %d tools for session %s → %s", toolCount, sess.SessionID, path)
	} else {
		log.Printf("[tool-store] provisioned instance %s opt-ins for session %s → %s", sess.InstanceID, sess.SessionID, path)
	}
	return nil
}

func (s *Server) replaceHarnessConfig(sess *store.Session, cfg map[string]json.RawMessage) error {
	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal HarnessConfig: %w", err)
	}
	sess.HarnessConfig = merged
	return nil
}

// writeProvisionedMCPConfig posts body to tool-store's /provision and writes
// the returned MCP server config to a tmpfile, returning its path. An empty
// returned path means tool-store had no servers to hand back — no file is
// written, because an --mcp-config pointing at an empty server set is a way to
// make a harness fail on nothing.
func (s *Server) writeProvisionedMCPConfig(body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal provision request: %w", err)
	}
	url := s.cfg.ToolStoreURL + "/provision"
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build provision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read tool-store response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tool-store /provision returned %d: %s", resp.StatusCode, string(respBody))
	}

	var provisioned struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(respBody, &provisioned); err != nil {
		return "", fmt.Errorf("tool-store /provision returned unparseable JSON: %w", err)
	}
	if len(provisioned.MCPServers) == 0 {
		return "", nil
	}

	dir := filepath.Join(os.TempDir(), "llm-bridge-mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create mcp tmpfile: %w", err)
	}
	if _, err := f.Write(respBody); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write mcp tmpfile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close mcp tmpfile: %w", err)
	}
	return f.Name(), nil
}
