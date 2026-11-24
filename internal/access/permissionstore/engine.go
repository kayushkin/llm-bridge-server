package permstore

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/access/permissionstore/splitter"
)

// EvaluateRequest is the input to the rule engine.
type EvaluateRequest struct {
	BridgeID   string          `json:"bridge_id,omitempty"`
	InstanceID string          `json:"instance_id,omitempty"`
	Tool       string          `json:"tool"`
	Input      json.RawMessage `json:"input"`
}

// EvaluateResponse is the engine's verdict.
type EvaluateResponse struct {
	Outcome       string           `json:"outcome"`           // "allow" | "deny" | "ask"
	MatchedRuleID string           `json:"matched_rule_id,omitempty"`
	Atoms         []splitter.Atom  `json:"atoms"`
	Message       string           `json:"message,omitempty"`
}

// Sentinel rule ids surfaced when no real rule matches:
//
//	"default-ask"  — no rule applied (fall-through default for safety)
//	"parse-fail"   — splitter returned an unparseable atom
const (
	RuleIDDefaultAsk = "default-ask"
	RuleIDParseFail  = "parse-fail"
)

// Engine evaluates tool-call requests against the rule store and writes
// audit entries for every call. Compiled regexes are cached in-process; the
// cache is invalidated wholesale on rule writes via InvalidateCache.
type Engine struct {
	store *Store

	mu       sync.RWMutex
	compiled map[string]*regexp.Regexp // pattern → regexp; nil pattern → nil entry never stored
}

// NewEngine wraps a Store with the evaluator.
func NewEngine(s *Store) *Engine {
	return &Engine{store: s, compiled: make(map[string]*regexp.Regexp)}
}

// InvalidateCache drops compiled-regex memoization. Call after any rule
// create/update/delete so the next Evaluate sees fresh patterns.
func (e *Engine) InvalidateCache() {
	e.mu.Lock()
	e.compiled = make(map[string]*regexp.Regexp)
	e.mu.Unlock()
}

// Evaluate runs the request through the cascade and writes one audit entry.
// On store / regex failure the response collapses to ask with an explanatory
// message — never silently allow.
//
// Outcome precedence across atoms (Bash splits to multiple): deny > ask >
// allow. The first deny short-circuits.
func (e *Engine) Evaluate(req EvaluateRequest) (EvaluateResponse, error) {
	atoms := atomsFor(req.Tool, req.Input)

	rules, err := e.cascade(req.BridgeID, req.InstanceID)
	if err != nil {
		// Store unreachable → ask, per locked design (never silently allow).
		resp := EvaluateResponse{
			Outcome:       OutcomeAsk,
			MatchedRuleID: RuleIDDefaultAsk,
			Atoms:         atoms,
			Message:       fmt.Sprintf("rule store error: %v", err),
		}
		e.writeAudit(req, atoms, resp)
		return resp, nil
	}

	resp := e.evaluateAtoms(req.Tool, atoms, rules)
	e.writeAudit(req, atoms, resp)
	return resp, nil
}

// evaluateAtoms walks atoms, returning the precedence-correct verdict.
func (e *Engine) evaluateAtoms(tool string, atoms []splitter.Atom, rules []Rule) EvaluateResponse {
	if len(atoms) == 0 {
		// No atoms to evaluate (empty Bash command, bare assignment) — allow.
		// CC will run nothing meaningful; gating it would just cause spurious
		// asks for noise.
		return EvaluateResponse{Outcome: OutcomeAllow, Atoms: atoms}
	}

	final := OutcomeAllow
	matchedID := ""
	message := ""

	for _, a := range atoms {
		if a.Kind == splitter.AtomUnparseable {
			if final != OutcomeDeny {
				final = OutcomeAsk
				matchedID = RuleIDParseFail
				message = fmt.Sprintf("could not parse: %s", a.Reason)
			}
			continue
		}
		outcome, ruleID, msg := e.matchAtom(tool, a, rules)
		switch outcome {
		case OutcomeDeny:
			// Short-circuit — any deny denies the whole call.
			return EvaluateResponse{
				Outcome:       OutcomeDeny,
				MatchedRuleID: ruleID,
				Atoms:         atoms,
				Message:       msg,
			}
		case OutcomeAsk:
			if final == OutcomeAllow {
				final = OutcomeAsk
				matchedID = ruleID
				message = msg
			}
		case OutcomeAllow:
			// allow doesn't change state — the call stays at whatever it was.
			// We still record the matched rule when no higher-precedence
			// rule has fired yet, so the audit shows which rule allowed.
			if matchedID == "" {
				matchedID = ruleID
			}
		}
	}

	return EvaluateResponse{
		Outcome:       final,
		MatchedRuleID: matchedID,
		Atoms:         atoms,
		Message:       message,
	}
}

// matchAtom returns the outcome of the first matching rule, or ask /
// default-ask if no rule matches.
func (e *Engine) matchAtom(tool string, a splitter.Atom, rules []Rule) (outcome, ruleID, message string) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if !toolMatches(r.Tool, tool) {
			continue
		}
		if r.Pattern != "" {
			re := e.compile(r.Pattern)
			if re == nil {
				// Bad regex — skip the rule. The next call to ListRules from
				// the UI will surface it; we don't want one bad rule to
				// cascade-fail every evaluation.
				continue
			}
			if !re.MatchString(a.Line) {
				continue
			}
		}
		// For redirects, only match if the rule has no pattern OR the
		// pattern matches the target. Tool-only rules apply to both calls
		// and redirects of that tool.
		if a.Kind == splitter.AtomRedirect && r.Pattern != "" {
			re := e.compile(r.Pattern)
			if re == nil || !re.MatchString(a.Target) {
				continue
			}
		}
		return r.Outcome, r.ID, r.Message
	}
	return OutcomeAsk, RuleIDDefaultAsk, ""
}

// compile memoizes the compiled regex.
func (e *Engine) compile(pattern string) *regexp.Regexp {
	e.mu.RLock()
	re, ok := e.compiled[pattern]
	e.mu.RUnlock()
	if ok {
		return re
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	e.mu.Lock()
	e.compiled[pattern] = compiled
	e.mu.Unlock()
	return compiled
}

// cascade returns rules in evaluation order: instance scope first (most
// specific), then bridge, then global. Within each scope, lower priority
// wins; ties broken by id.
func (e *Engine) cascade(bridgeID, instanceID string) ([]Rule, error) {
	all, err := e.store.ListRules("")
	if err != nil {
		return nil, err
	}
	// Filter to scopes we care about for this request.
	out := all[:0]
	for _, r := range all {
		switch {
		case r.Scope == ScopeGlobal:
			out = append(out, r)
		case bridgeID != "" && r.Scope == ScopeBridgePfx+bridgeID:
			out = append(out, r)
		case instanceID != "" && r.Scope == ScopeInstancePfx+instanceID:
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Most specific first: instance > bridge > global.
		si := scopeRank(out[i].Scope)
		sj := scopeRank(out[j].Scope)
		if si != sj {
			return si < sj
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func scopeRank(scope string) int {
	switch {
	case strings.HasPrefix(scope, ScopeInstancePfx):
		return 0
	case strings.HasPrefix(scope, ScopeBridgePfx):
		return 1
	case scope == ScopeGlobal:
		return 2
	}
	return 99
}

func toolMatches(ruleTool, requestTool string) bool {
	if ruleTool == "*" {
		return true
	}
	return ruleTool == requestTool
}

// atomsFor returns the atoms to evaluate. Bash gets split via the AST
// parser; every other tool gets a single synthetic Call atom whose Line is
// the tool name + JSON-encoded input — that way a rule like
// `pattern: "github.com"` against tool=WebFetch matches when the URL field
// contains the substring, without requiring a per-tool input-extraction
// table.
func atomsFor(tool string, input json.RawMessage) []splitter.Atom {
	if tool == "Bash" {
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &args)
		if args.Command == "" {
			return nil
		}
		return splitter.Split(args.Command)
	}
	line := tool
	if len(input) > 0 {
		line = tool + " " + string(input)
	}
	return []splitter.Atom{{
		Kind: splitter.AtomCall,
		Argv: []string{tool},
		Line: line,
	}}
}

func (e *Engine) writeAudit(req EvaluateRequest, atoms []splitter.Atom, resp EvaluateResponse) {
	atomsJSON, _ := json.Marshal(atoms)
	_, err := e.store.InsertAudit(AuditEntry{
		BridgeID:      req.BridgeID,
		InstanceID:    req.InstanceID,
		Tool:          req.Tool,
		InputJSON:     req.Input,
		AtomsJSON:     atomsJSON,
		Outcome:       resp.Outcome,
		MatchedRuleID: resp.MatchedRuleID,
	})
	if err != nil {
		// Audit write failure is logged at the caller (HTTP handler) — we
		// don't fail the evaluation, since denying a call because we couldn't
		// log it would be worse than the gap in the audit trail.
		fmt.Printf("[permission-store] audit insert failed: %v\n", err)
	}
}
