// Package bundlestore is the canonical registry of curated session "bundles":
// named, inheritable sets of skills + tools (with optional model/effort
// defaults) that get selected for a session by repository and task tags. A
// bundle is a cross-store join artifact — its members are the ids skill-store
// and tool-store assigned, carried with the name alongside for display.
// bundle-store owns the curated definitions and the /resolve composition; it
// does not own the underlying skills/tools and is owned by no harness.
//
// Nothing in this package looks a member up by name. Names are not unique in
// skill-store, whose sources are separate upstream repositories, so the id an
// author supplies is the only thing that identifies a member. Whoever writes a
// bundle reads the id out of the owning store; see cmd/bundle-store for the
// one place on this side that turns a curated name into an id, and for how
// loudly it gives up when a name is ambiguous.
package bundlestore

type MemberKind string

const (
	// MemberSkill resolves against skill-store (a SKILL.md name).
	MemberSkill MemberKind = "skill"
	// MemberTool resolves against tool-store (an mcp/cli/local tool name).
	MemberTool MemberKind = "tool"
)

func (k MemberKind) Valid() bool { return k == MemberSkill || k == MemberTool }

// OwningStore names the service that assigned a member's id, for error
// messages that have to tell an author where to go and read it.
func (k MemberKind) OwningStore() string {
	switch k {
	case MemberSkill:
		return "skill-store"
	case MemberTool:
		return "tool-store"
	}
	return "unknown store"
}

// MemberEffect says what a member does to a session. An included member is
// added to it. A denied member is taken away from it, even when another bundle
// in the same resolution includes it, and even when the tool is one a harness
// turns on by default (tool-store's kind "harness", such as claude_code.Write).
// A tool is either added or denied; there is no third "restricted" state.
type MemberEffect string

const (
	// MemberInclude adds the member to the session. It is what a member
	// written without an effect gets.
	MemberInclude MemberEffect = "include"
	// MemberDeny removes a tool from the session. Only tools can be denied: a
	// skill is installed or it is not, and nothing turns one on by default.
	MemberDeny MemberEffect = "deny"
)

// MemberEffects is every effect a member may carry, in the order
// GET /member-effects serves them. Validation reads this list and nothing else.
var MemberEffects = []MemberEffect{MemberInclude, MemberDeny}

// Valid reports whether e is one of MemberEffects.
func (e MemberEffect) Valid() bool {
	for _, known := range MemberEffects {
		if e == known {
			return true
		}
	}
	return false
}

// AllowedFor reports whether a member of this kind may carry e.
func (e MemberEffect) AllowedFor(kind MemberKind) bool {
	return e != MemberDeny || kind == MemberTool
}

// Member is one skill or tool a bundle contributes. ID is the identifier the
// owning store assigned — skill-store's for MemberSkill, tool-store's for
// MemberTool — and is what a consumer resolves the member with. Name travels
// with it as a display label and is never read back to find the member.
// Condition, if set, is a task tag that gates the member: it only takes effect
// when a resolve request's task_tags contain it, whether it includes or denies.
// Effect is MemberInclude or MemberDeny; a write that leaves it empty stores
// MemberInclude, and every read returns it filled in.
type Member struct {
	Kind      MemberKind   `json:"kind"`
	ID        int64        `json:"id"`
	Name      string       `json:"name,omitempty"`
	Condition string       `json:"condition,omitempty"`
	Effect    MemberEffect `json:"effect"`
}

// MemberRef is a resolved member on the wire: the owning store's id, plus the
// name for display. A consumer installs a skill with skill-store's
// POST /skills/{id}/install and provisions a tool by tool-store id — neither
// needs the name, and neither should search by it.
type MemberRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// BundleRef names one bundle that contributed to a resolution, by bundle-store
// id with its name for display.
type BundleRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Bundle is a curated definition. MatchTags select it against a repo/task tag
// set. DeniedReadPaths are path patterns a session given this bundle may not
// read: each starts with "/" or "~/", and may use "*" and "**" globs. They are
// plain paths, not any one harness's rule syntax; a harness wrapper turns them
// into its own form. ExtendsID points at a parent bundle whose members are inherited
// (ancestor members first, child overrides/extends). Model/Effort are optional
// session defaults; agent-store config wins on conflict (see README).
//
// ExtendsID is the inheritance link; Extends is the parent's name, carried
// alongside it for display exactly as Member.Name is. The chain walk reads
// ExtendsID and never Extends.
//
// On a write either one may be supplied. A caller that has the parent's id
// gives ExtendsID; a caller writing a definition by hand (a curl, a script)
// gives Extends, and UpsertBundle turns that name into an id once, against
// bundle-store's own bundles table, refusing the write if no such parent exists.
// Supplying both is only accepted when they agree — one fact arriving by two
// routes has to be checked, not silently resolved in favor of either.
type Bundle struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description,omitempty"`
	ExtendsID   int64    `json:"extends_id,omitempty"`
	Extends     string   `json:"extends,omitempty"`
	MatchTags   []string `json:"match_tags,omitempty"`
	Members     []Member `json:"members,omitempty"`
	// DeniedReadPaths lists path patterns the session may not read.
	DeniedReadPaths []string `json:"denied_read_paths,omitempty"`
	Model           string   `json:"model,omitempty"`
	Effort          string   `json:"effort,omitempty"`
	Enabled         bool     `json:"enabled"`
	CreatedAt       int64    `json:"created_at"`
	UpdatedAt       int64    `json:"updated_at"`
}
