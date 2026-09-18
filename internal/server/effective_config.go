package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The effective config view: what a session is given, setting by setting,
// with the layer that decided each. Two routes serve it —
//
//	GET /sessions/{id}/effective-config   for a stored session
//	GET /effective-config?harness=…       for a session that does not exist yet
//
// — and both read the same code the spawn reads (resolveModelSelection,
// resolveToolOffer, the grant gate's reads, the permission-mode snapshot), so
// the view cannot say one thing while the spawn does another. It reports and
// decides nothing: no config file is written, no session is created.

// inputOrigin says where a dry run's input came from, so the view attributes
// an instance chosen by a board to the board and not to the caller.
type inputOrigin struct {
	Layer  msg.EffectiveConfigLayer
	Record string
	Detail string
}

// effectiveConfigFor computes the view for sess. origins carries, per input
// setting (principal, instance, agent, bundle), where a dry run took it from;
// a stored session's inputs were all named at create and default to
// EffectiveLayerRequest.
func (s *Server) effectiveConfigFor(ctx context.Context, sess *store.Session, subject msg.EffectiveConfigSubject, origins map[msg.EffectiveSettingKey]inputOrigin) *msg.EffectiveConfig {
	out := &msg.EffectiveConfig{Subject: subject, Layers: msg.EffectiveConfigLayers, Warnings: []string{}}
	warn := func(format string, args ...any) { out.Warnings = append(out.Warnings, fmt.Sprintf(format, args...)) }
	add := func(setting msg.EffectiveSetting) { out.Settings = append(out.Settings, setting) }

	cfg, err := parseHarnessConfig(sess)
	if err != nil {
		warn("harness_config could not be read, so the settings it carries are reported as unset: %v", err)
		cfg = map[string]json.RawMessage{}
	}
	var defaults msg.HarnessDefaults
	var prefs msg.BridgePrefs
	if s.bridgePrefs != nil {
		prefs = s.bridgePrefs.get()
		defaults = prefs.Defaults[string(sess.Harness)]
	}
	harnessRecord := func(field string) string { return fmt.Sprintf("bridge-prefs.defaults.%s.%s", sess.Harness, field) }
	originOf := func(key msg.EffectiveSettingKey, fallbackDetail string) inputOrigin {
		if o, ok := origins[key]; ok {
			return o
		}
		return inputOrigin{Layer: msg.EffectiveLayerRequest, Detail: fallbackDetail}
	}

	// model
	add(s.effectiveModel(ctx, sess, cfg, harnessRecord, subject.DryRun, warn))

	// effort, max_budget, disabled_tools: decided at create by one precedence —
	// the session's own value, then its bundle, then the harness default — and
	// pinned with the layer that decided each (session_defaults.go).
	pinnedLayers := settingLayersPinnedOn(cfg)
	describePinned := func(setting *msg.EffectiveSetting, key msg.EffectiveSettingKey, sessionRecord, defaultsField, sessionDetail string) {
		layer, recorded := pinnedLayers[key]
		switch {
		case !recorded:
			setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerSession, sessionRecord, sessionDetail
			setting.Notes = append(setting.Notes, "the row predates the record of which layer set this (2026-09-18), so it is reported as the session's own")
		case layer == msg.EffectiveLayerBundle:
			setting.Layer, setting.Record = layer, fmt.Sprintf("bundle-store bundle %s", sess.BundleID)
			setting.Detail = "the creator named none, so the session's bundle decided it"
		case layer == msg.EffectiveLayerHarnessDefault:
			setting.Layer, setting.Record = layer, harnessRecord(defaultsField)
			setting.Detail = "the creator named none and the bundle names none, so the harness default on the Settings page decided it"
		default:
			setting.Layer, setting.Record, setting.Detail = layer, sessionRecord, sessionDetail
		}
		if !subject.DryRun && recorded && layer != msg.EffectiveLayerSession {
			setting.Notes = append(setting.Notes, "pinned at create; a later change to the bundle or the harness default does not move it")
		}
	}

	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingEffort, Layer: msg.EffectiveLayerNone, Detail: "nothing sets it; the harness runs at its own default effort"}
		if raw, ok := cfg[harnessConfigKeyEffort]; ok {
			var effort string
			if json.Unmarshal(raw, &effort) == nil && effort != "" {
				setting.Value = effort
				describePinned(&setting, msg.EffectiveSettingEffort, "harness_config.effort", "effort", "set on the session at create or by POST /sessions/{id}/config")
			}
		}
		if defaults.Effort != "" && setting.Layer != msg.EffectiveLayerHarnessDefault && setting.Value != defaults.Effort {
			setting.Notes = append(setting.Notes, fmt.Sprintf("%s is %q and was outranked", harnessRecord("effort"), defaults.Effort))
		}
		add(setting)
	}

	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingMaxBudget, Layer: msg.EffectiveLayerNone, Detail: "no ceiling"}
		_, recorded := pinnedLayers[msg.EffectiveSettingMaxBudget]
		if sess.MaxBudgetUSD > 0 || recorded {
			setting.Value = sess.MaxBudgetUSD
			describePinned(&setting, msg.EffectiveSettingMaxBudget, "sessions.max_budget_usd", "max_budget", "the session's spend ceiling; the spend-ceiling guard holds the session when it is reached")
			if sess.MaxBudgetUSD == 0 {
				setting.Notes = append(setting.Notes, "0 means no ceiling")
			}
		}
		if defaults.MaxBudget != nil && setting.Layer != msg.EffectiveLayerHarnessDefault && setting.Value != *defaults.MaxBudget {
			setting.Notes = append(setting.Notes, fmt.Sprintf("%s is %v and was outranked", harnessRecord("max_budget"), *defaults.MaxBudget))
		}
		add(setting)
	}

	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingDisabledTools, Layer: msg.EffectiveLayerNone, Detail: "nothing disables a built-in tool"}
		if raw, ok := cfg[harnessConfigKeyDisabledTools]; ok {
			var tools []string
			if json.Unmarshal(raw, &tools) == nil {
				setting.Value = tools
				describePinned(&setting, msg.EffectiveSettingDisabledTools, "harness_config.disabled_tools", "disabled_tools", "built-in tools the harness is told not to offer, set on the session")
			}
		}
		if len(defaults.DisabledTools) > 0 && setting.Layer == msg.EffectiveLayerSession {
			setting.Notes = append(setting.Notes, fmt.Sprintf("%s is %v; the session's own list replaces it", harnessRecord("disabled_tools"), defaults.DisabledTools))
		}
		add(setting)
	}

	// permission_mode
	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingPermissionMode}
		globalMode := ""
		if s.bridgePrefs != nil {
			globalMode = s.globalPermissionMode()
		}
		if raw, ok := cfg["permission_mode"]; ok {
			var mode string
			_ = json.Unmarshal(raw, &mode)
			setting.Value, setting.Record = mode, "harness_config.permission_mode"
			if subject.DryRun {
				setting.Layer = msg.EffectiveLayerGlobal
				setting.Record = "bridge-prefs.permission_mode"
				setting.Detail = "the global mode, which POST /sessions pins into the session at create"
			} else {
				setting.Layer = msg.EffectiveLayerSession
				setting.Detail = "pinned at create from the global mode unless the caller set one; the chat header changes it per session, and a later change to the global mode does not move it"
				if globalMode != "" && globalMode != mode {
					setting.Notes = append(setting.Notes, fmt.Sprintf("the global mode is now %q", globalMode))
				}
			}
		} else if globalMode != "" {
			setting.Value, setting.Layer, setting.Record = globalMode, msg.EffectiveLayerGlobal, "bridge-prefs.permission_mode"
			setting.Detail = "the row carries no mode of its own (it predates the snapshot), so the prehook reads the global mode on every call"
		} else {
			setting.Layer = msg.EffectiveLayerNone
			setting.Detail = "no bridge-prefs are configured on this server"
		}
		if setting.Value == msg.PermissionModeBypass {
			setting.Notes = append(setting.Notes, "bypass: every tool call is allowed and permission-store's rules are not consulted")
		}
		add(setting)
	}

	// principal
	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingPrincipal}
		if sess.PrincipalID != "" {
			o := originOf(msg.EffectiveSettingPrincipal, "sent by whoever created the session")
			setting.Value, setting.Layer, setting.Record, setting.Detail = sess.PrincipalID, o.Layer, o.Record, o.Detail
			if setting.Record == "" {
				setting.Record = "sessions.principal_id"
			}
			if prefs.DefaultPrincipalID != "" && prefs.DefaultPrincipalID == sess.PrincipalID && o.Layer == msg.EffectiveLayerRequest {
				setting.Notes = append(setting.Notes, "equals bridge-prefs.default_principal_id, which the chat pane sends unless the creator chose otherwise")
			}
		} else {
			setting.Layer, setting.Detail = msg.EffectiveLayerNone, "no principal: grants are not consulted and the instance's opt-ins are offered unchanged"
			if prefs.DefaultPrincipalID != "" {
				setting.Notes = append(setting.Notes, fmt.Sprintf("bridge-prefs.default_principal_id is %s; the creator did not send it (the chat pane would have)", prefs.DefaultPrincipalID))
			}
		}
		add(setting)
	}

	// instance, with the can_dispatch_on gate's reads
	var inst *msg.Instance
	{
		o := originOf(msg.EffectiveSettingInstance, "named at create, or the one enabled instance of the harness when the creator named none")
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingInstance, Value: sess.InstanceID, Layer: o.Layer, Record: o.Record, Detail: o.Detail}
		if setting.Record == "" {
			setting.Record = "sessions.instance_id"
		}
		if sess.InstanceID == "" {
			setting.Value, setting.Layer, setting.Detail = nil, msg.EffectiveLayerNone, "no instance"
		} else if s.harnessStore != nil {
			if loaded, err := s.harnessStore.GetInstance(sess.InstanceID); err == nil {
				inst = loaded
				setting.Notes = append(setting.Notes, fmt.Sprintf("harness %s on machine %s", inst.HarnessType, inst.MachineID))
			} else {
				warn("instance %s is not in harness-store: %v", sess.InstanceID, err)
			}
		}
		if sess.PrincipalID != "" && inst != nil {
			s.noteDispatchGrants(ctx, sess, inst, &setting, warn)
		}
		add(setting)
	}

	// agent, with the can_run_as gate's reads
	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingAgent, Layer: msg.EffectiveLayerNone, Detail: "no agent identity; the harness runs as itself"}
		if sess.AgentID != "" {
			o := originOf(msg.EffectiveSettingAgent, "named at create")
			setting.Value, setting.Layer, setting.Record, setting.Detail = sess.AgentID, o.Layer, o.Record, o.Detail
			if setting.Record == "" {
				setting.Record = "sessions.agent_id"
			}
			if sess.PrincipalID != "" {
				s.noteRunAsGrants(ctx, sess, &setting, warn)
			}
		}
		add(setting)
	}

	// bundle
	{
		setting := msg.EffectiveSetting{Key: msg.EffectiveSettingBundle, Layer: msg.EffectiveLayerNone, Detail: "no bundle; tools come from grants or the instance's opt-ins"}
		if sess.BundleID != "" {
			o := originOf(msg.EffectiveSettingBundle, "named at create — normally a kanban board's default_bundle_id")
			setting.Value, setting.Layer, setting.Record, setting.Detail = sess.BundleID, o.Layer, o.Record, o.Detail
			if setting.Record == "" {
				setting.Record = "sessions.bundle_id"
			}
			if s.bundleClient == nil {
				warn("the session names bundle %s but this server has no bundle-store to resolve it with (LLMBRIDGE_BUNDLE_STORE_URL); a spawn would be refused", sess.BundleID)
			} else if resolution, err := s.bundleClient.Resolve(ctx, sess.BundleID); err != nil {
				warn("bundle %s could not be resolved, so a spawn would be refused: %v", sess.BundleID, err)
			} else {
				names := make([]string, 0, len(resolution.Bundles))
				for _, b := range resolution.Bundles {
					names = append(names, fmt.Sprintf("%s (#%d)", b.Name, b.ID))
				}
				setting.Notes = append(setting.Notes, fmt.Sprintf("resolves to bundles %s: %d tools, %d skills", strings.Join(names, ", "), len(resolution.Tools), len(resolution.Skills)))
				if resolution.Model != "" || resolution.Effort != "" {
					setting.Notes = append(setting.Notes, fmt.Sprintf("the bundle names model %q and effort %q; each applies at create unless the creator named its own, and outranks the harness default", resolution.Model, resolution.Effort))
				}
			}
		}
		add(setting)
	}

	// tools: the same decision the spawn makes, unprovisioned
	add(s.effectiveTools(ctx, sess, cfg, warn))

	return out
}

func (s *Server) effectiveModel(ctx context.Context, sess *store.Session, cfg map[string]json.RawMessage, harnessRecord func(string) string, dryRun bool, warn func(string, ...any)) msg.EffectiveSetting {
	setting := msg.EffectiveSetting{Key: msg.EffectiveSettingModel}
	var selection msg.ModelSelection
	pinned := false
	if raw, ok := cfg[harnessConfigKeyModelSelection]; ok && json.Unmarshal(raw, &selection) == nil && selection.Model != "" {
		pinned = true
	} else {
		resolved, err := s.resolveModelSelection(ctx, sess)
		if err != nil {
			warn("the model could not be resolved, so a spawn would be refused: %v", err)
			setting.Layer, setting.Detail = msg.EffectiveLayerNone, "no layer can answer"
			return setting
		}
		selection = resolved
		setting.Notes = append(setting.Notes, "not pinned on this row; resolved now exactly as a spawn would")
	}
	setting.Value = string(selection.Model)
	switch selection.SelectedBy {
	case msg.ModelSelectedBySession:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerSession, "harness_config.model", "the session's own choice, at create or from the chat header's picker"
	case msg.ModelSelectedByBundle:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerBundle, fmt.Sprintf("bundle-store bundle %s", sess.BundleID), "no session choice, so the model the session's bundle names"
	case msg.ModelSelectedByPrefs:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerHarnessDefault, harnessRecord("model"), "no session choice and no bundle model, so the harness default on the Settings page"
	case msg.ModelSelectedByRole:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerRegistry, "model-store role \"default\"", "no session choice, no bundle model and no harness default, so the registry's default role"
	default:
		setting.Layer, setting.Detail = msg.EffectiveLayerSession, fmt.Sprintf("selected_by %q", selection.SelectedBy)
	}
	if selection.Role != "" {
		setting.Notes = append(setting.Notes, fmt.Sprintf("asked for as role %q", selection.Role))
	}
	if pinned && !dryRun {
		setting.Notes = append(setting.Notes, "pinned into harness_config.model_selection at create; a later change to the bundle or the harness default does not move it")
	}
	return setting
}

func (s *Server) effectiveTools(ctx context.Context, sess *store.Session, cfg map[string]json.RawMessage, warn func(string, ...any)) msg.EffectiveSetting {
	setting := msg.EffectiveSetting{Key: msg.EffectiveSettingTools}
	if sess.Harness != msg.HarnessClaudeCode {
		setting.Layer, setting.Detail = msg.EffectiveLayerNone, fmt.Sprintf("MCP tools are provisioned for claude_code only; %s sessions are handed none", sess.Harness)
		return setting
	}
	if s.cfg == nil || s.cfg.ToolStoreURL == "" {
		setting.Layer, setting.Detail = msg.EffectiveLayerNone, "this server has no tool-store (LLMBRIDGE_TOOL_STORE_URL), so no MCP tools are provisioned"
		return setting
	}
	offer, err := s.resolveToolOffer(ctx, sess, cfg)
	if err != nil {
		warn("the tool offer could not be decided, so a spawn would be refused: %v", err)
		setting.Layer, setting.Detail = msg.EffectiveLayerNone, "a spawn would be refused; see warnings"
		return setting
	}
	setting.Notes = offer.Notes
	switch offer.Source {
	case toolOfferPreset:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerSession, "harness_config.mcp_config", "the caller wrote the MCP config outright"
		setting.Value = json.RawMessage(cfg["mcp_config"])
	case toolOfferHandNamed:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerSession, "harness_config.tool_store_tools", "the caller named these tools by hand; they are provisioned by name"
		setting.Value = offer.Names
	case toolOfferBundle:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerBundle, fmt.Sprintf("bundle-store bundle %s", sess.BundleID), "the bundle's tools, provisioned by id; the instance's opt-ins are not consulted"
		if offer.NarrowedByGrants {
			setting.Layer, setting.Detail = msg.EffectiveLayerGrant, "the bundle's tools that the principal's can_use grants also name"
		}
		setting.Value = toolValues(offer.IDs, toolNames(offer))
	case toolOfferGrants:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerGrant, fmt.Sprintf("grant-store can_use for %s ∩ tool-store opt-ins of %s", sess.PrincipalID, sess.InstanceID), "what the principal is granted and the instance has opted in"
		setting.Value = toolValues(offer.IDs, toolNames(offer))
	case toolOfferInstance:
		setting.Layer, setting.Record, setting.Detail = msg.EffectiveLayerInstance, fmt.Sprintf("tool-store instance_tools for %s", sess.InstanceID), "the instance's opt-ins, the rows the Tools page writes"
		setting.Value = toolValues(offer.IDs, toolNames(offer))
	default:
		setting.Layer, setting.Detail = msg.EffectiveLayerNone, "no MCP tools"
	}
	return setting
}

// toolNames is what the offer knows about its tools' names: a bundle
// resolution carries them; an opt-in or grant answer carries ids only.
func toolNames(offer *toolOffer) map[int64]string {
	names := map[int64]string{}
	if offer.Resolution != nil {
		for _, t := range offer.Resolution.Tools {
			names[t.ID] = t.Name
		}
	}
	return names
}

func toolValues(ids []int64, names map[int64]string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row := map[string]any{"id": id}
		if name, ok := names[id]; ok {
			row["name"] = name
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) noteDispatchGrants(ctx context.Context, sess *store.Session, inst *msg.Instance, setting *msg.EffectiveSetting, warn func(string, ...any)) {
	if s.grantClient == nil {
		warn("the session names principal %s but this server has no grant-store (LLMBRIDGE_GRANT_STORE_URL); a spawn would be refused", sess.PrincipalID)
		return
	}
	instanceIDs, err := s.grantClient.EffectiveResourceIDs(ctx, sess.PrincipalID, "can_dispatch_on", "instance")
	if err != nil {
		warn("%s's can_dispatch_on grants could not be read; a spawn would be refused: %v", sess.PrincipalID, err)
		return
	}
	machineIDs, err := s.grantClient.EffectiveResourceIDs(ctx, sess.PrincipalID, "can_dispatch_on", "machine")
	if err != nil {
		warn("%s's can_dispatch_on grants could not be read; a spawn would be refused: %v", sess.PrincipalID, err)
		return
	}
	switch {
	case len(instanceIDs)+len(machineIDs) == 0:
		setting.Notes = append(setting.Notes, fmt.Sprintf("%s holds no can_dispatch_on grant, so any instance is allowed (lenient)", sess.PrincipalID))
	case contains(instanceIDs, inst.ID) || contains(machineIDs, inst.MachineID):
		setting.Notes = append(setting.Notes, fmt.Sprintf("allowed by %s's can_dispatch_on grants (instances %v, machines %v)", sess.PrincipalID, instanceIDs, machineIDs))
	default:
		warn("a spawn would be refused: %s may not start a session on instance %s (machine %s); their can_dispatch_on grants name instances %v and machines %v", sess.PrincipalID, inst.ID, inst.MachineID, instanceIDs, machineIDs)
	}
}

func (s *Server) noteRunAsGrants(ctx context.Context, sess *store.Session, setting *msg.EffectiveSetting, warn func(string, ...any)) {
	if s.grantClient == nil {
		return
	}
	agentIDs, err := s.grantClient.EffectiveResourceIDs(ctx, sess.PrincipalID, "can_run_as", "agent")
	if err != nil {
		warn("%s's can_run_as grants could not be read; a spawn would be refused: %v", sess.PrincipalID, err)
		return
	}
	if len(agentIDs) == 0 {
		setting.Notes = append(setting.Notes, fmt.Sprintf("%s holds no can_run_as grant, so any agent is allowed (lenient)", sess.PrincipalID))
		return
	}
	agentID, err := s.resolveAgentID(sess.AgentID)
	if err != nil {
		warn("a spawn would be refused: %s holds can_run_as grants for agents %v and agent %q could not be resolved to an agent-store id: %v", sess.PrincipalID, agentIDs, sess.AgentID, err)
		return
	}
	if contains(agentIDs, agentID) {
		setting.Notes = append(setting.Notes, fmt.Sprintf("allowed by %s's can_run_as grants (agent-store id %s)", sess.PrincipalID, agentID))
	} else {
		warn("a spawn would be refused: %s may not run as agent %s (agent-store id %s); their can_run_as grants name %v", sess.PrincipalID, sess.AgentID, agentID, agentIDs)
	}
}

// handleSessionEffectiveConfig: GET /sessions/{id}/effective-config.
func (s *Server) handleSessionEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "session_not_found", fmt.Sprintf("no session %s", sessionID))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	subject := msg.EffectiveConfigSubject{
		SessionID: sess.SessionID, Harness: sess.Harness, InstanceID: sess.InstanceID,
		PrincipalID: sess.PrincipalID, AgentID: sess.AgentID, BundleID: sess.BundleID,
	}
	writeJSON(w, s.effectiveConfigFor(ctx, sess, subject, nil))
}

// handleDryRunEffectiveConfig: GET /effective-config?harness=…&instance_id=…
// &principal_id=…&agent_id=…&bundle_id=…&board_id=…&card_id=…&tag=…
//
// A board fills in whichever of principal, agent, instance and bundle the
// caller left empty, exactly as the kanban dispatcher does, and the answer
// says so. A principal the caller left empty is then filled from
// bridge-prefs.default_principal_id, as the chat pane does — a scripted
// caller that omits it gets none, and the answer says that too.
func (s *Server) handleDryRunEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	subject := msg.EffectiveConfigSubject{
		DryRun: true, Harness: msg.Harness(q.Get("harness")), InstanceID: q.Get("instance_id"),
		PrincipalID: q.Get("principal_id"), AgentID: q.Get("agent_id"), BundleID: q.Get("bundle_id"),
		BoardID: q.Get("board_id"), CardID: q.Get("card_id"), Tags: q["tag"],
	}
	origins := map[msg.EffectiveSettingKey]inputOrigin{}
	for key, value := range map[msg.EffectiveSettingKey]string{
		msg.EffectiveSettingPrincipal: subject.PrincipalID, msg.EffectiveSettingInstance: subject.InstanceID,
		msg.EffectiveSettingAgent: subject.AgentID, msg.EffectiveSettingBundle: subject.BundleID,
	} {
		if value != "" {
			origins[key] = inputOrigin{Layer: msg.EffectiveLayerRequest, Detail: "named in the query, as a creator would name it"}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if subject.BoardID != "" {
		if s.kanbanClient == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "kanban_store_not_configured", "board_id was given but this server has no kanban-store to read the board's defaults from (LLMBRIDGE_KANBAN_STORE_URL)")
			return
		}
		resolved, err := s.kanbanClient.EffectiveDefaults(ctx, subject.BoardID, subject.CardID, subject.Tags)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "kanban_store_unavailable", fmt.Sprintf("kanban-store could not answer for board %s: %v", subject.BoardID, err))
			return
		}
		apply := func(key msg.EffectiveSettingKey, field string, target *string) {
			d, ok := resolved.Defaults[field]
			if !ok || d.Value == "" || *target != "" {
				return
			}
			*target = d.Value
			origin := inputOrigin{Layer: msg.EffectiveLayerBoard, Record: fmt.Sprintf("kanban-store board %s.%s", subject.BoardID, field), Detail: "the board's default, which a dispatcher sends when it creates a session from a card"}
			if d.Source.Kind == "tag_rule" {
				origin.Layer = msg.EffectiveLayerTagRule
				origin.Record = fmt.Sprintf("kanban-store board %s tag rule %s (tags %v)", subject.BoardID, d.Source.RuleID, d.Source.RuleTags)
				origin.Detail = "a tag rule matched the card's tags and outranks the board's default"
			}
			origins[key] = origin
		}
		apply(msg.EffectiveSettingPrincipal, "default_principal_id", &subject.PrincipalID)
		apply(msg.EffectiveSettingAgent, "default_agent_id", &subject.AgentID)
		apply(msg.EffectiveSettingInstance, "default_instance_id", &subject.InstanceID)
		apply(msg.EffectiveSettingBundle, "default_bundle_id", &subject.BundleID)
	}
	if subject.PrincipalID == "" && s.bridgePrefs != nil {
		if id := s.bridgePrefs.get().DefaultPrincipalID; id != "" {
			subject.PrincipalID = id
			origins[msg.EffectiveSettingPrincipal] = inputOrigin{Layer: msg.EffectiveLayerGlobal, Record: "bridge-prefs.default_principal_id", Detail: "the chat pane sends this when the creator names none; a scripted caller that omits principal_id gets no principal"}
		}
	}

	if s.harnessStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "harness_store_not_configured", "harness-store not configured")
		return
	}
	if subject.InstanceID != "" && subject.Harness == "" {
		inst, err := s.harnessStore.GetInstance(subject.InstanceID)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "instance_not_found", fmt.Sprintf("instance %s is not in harness-store", subject.InstanceID))
			return
		}
		subject.Harness = inst.HarnessType
	}
	if !isValidHarness(subject.Harness) {
		writeJSONError(w, http.StatusBadRequest, "invalid_harness", "harness is required (or an instance_id to take it from)")
		return
	}
	inst, herr := resolveInstance(s.harnessStore, subject.InstanceID, subject.Harness)
	if herr != nil {
		writeJSONError(w, herr.status, "instance_unresolvable", herr.message)
		return
	}
	if subject.InstanceID == "" {
		origins[msg.EffectiveSettingInstance] = inputOrigin{Layer: msg.EffectiveLayerRequest, Detail: "the one enabled instance of the harness, chosen because the caller named none"}
	}
	subject.InstanceID = inst.ID

	sess := &store.Session{
		SessionID: "(dry run)", Harness: subject.Harness, InstanceID: inst.ID,
		PrincipalID: subject.PrincipalID, AgentID: subject.AgentID, BundleID: subject.BundleID,
	}
	s.snapshotPermissionModeIntoSession(sess)
	// What POST /sessions would pin for a creator who names no model, effort,
	// ceiling or disabled tools — which is what a dispatcher sends.
	snapshotErr := s.snapshotSessionDefaultsIntoSession(ctx, sess, false)
	answer := s.effectiveConfigFor(ctx, sess, subject, origins)
	if snapshotErr != nil {
		answer.Warnings = append(answer.Warnings, fmt.Sprintf("POST /sessions would refuse this session: %v", snapshotErr))
	}
	writeJSON(w, answer)
}
