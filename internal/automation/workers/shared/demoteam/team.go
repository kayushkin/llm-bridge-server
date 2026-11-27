// Package demoteam is the roster for the simulated Northwind team.
//
// One definition, because the team is referred to from several places that must
// agree: the mail generator writes as these people, the dispatcher runs agents
// as these identities, and the board attributes work to them. A second copy of
// the roster anywhere would drift, and the first symptom would be mail from a
// person the board has never heard of.
//
// The simulated humans and the working agents live in ONE identity space. In a
// simulation a "human" is an agent playing a human, so there is no second kind
// of thing to model — they are all agent-store rows with slugs, and the board
// links to any of them through the `agent` entity type it already registers.
package demoteam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Role is the governed vocabulary permissions key on.
//
// Deliberately the same five values kanban-scoper already defines, rather than
// a new set: it is the only enumerated agent-role vocabulary on this host, and
// two vocabularies for one concept is how a permission rule ends up matching
// nothing. agent-store's own `role` column is free prose rendered into a system
// prompt, and cannot carry authority.
type Role string

const (
	RoleBuilder    Role = "builder"    // writes code
	RoleReviewer   Role = "reviewer"   // reviews; does not write
	RoleTester     Role = "tester"     // exercises; does not write
	RoleResearcher Role = "researcher" // investigates and decides; does not write
	RoleDocs       Role = "docs"       // writes prose, not code
)

// Member is one identity on the team.
type Member struct {
	// Slug is the agent-store key and the ref everything joins on.
	Slug string
	Name string
	// Email is set for members who send mail, and empty for the working agents,
	// who exist to do work rather than to ask for it.
	Email string
	Emoji string
	// Voice describes how this member writes and what they care about. It is
	// prompt material — given to the model so a product manager does not sound
	// like a build robot — and it is what agent-store stores as `role`.
	Voice string
	// Role is the governed role. Stored as a `role:<value>` entity tag on
	// ("agent", Slug), which is where permissions will read it.
	Role Role
}

// SendsMail reports whether this member writes email. The working agents do not.
func (m Member) SendsMail() bool { return m.Email != "" }

// Roster is the whole team.
var Roster = []Member{
	{
		Slug: "northwind-priya", Name: "Priya Raman", Emoji: "📋",
		Email: "priya.raman@northwind-eng.example", Role: RoleResearcher,
		Voice: "product manager; asks for work, changes scope mid-flight, chases delivery dates",
	},
	{
		Slug: "northwind-dinesh", Name: "Dinesh Okonkwo", Emoji: "🔍",
		Email: "dinesh.okonkwo@northwind-eng.example", Role: RoleReviewer,
		Voice: "senior engineer who reviews pull requests; terse, technical, direct",
	},
	{
		Slug: "northwind-marcus", Name: "Marcus Feld", Emoji: "🏗️",
		Email: "marcus.feld@northwind-eng.example", Role: RoleResearcher,
		Voice: "platform lead; makes build and infrastructure decisions, cancels work that is not paying off",
	},
	{
		Slug: "northwind-helena", Name: "Helena Vos", Emoji: "📣",
		Email: "helena.vos@northwind-eng.example", Role: RoleResearcher,
		Voice: "engineering manager; asks for write-ups, incident reviews and status for people above her",
	},
	{
		Slug: "northwind-builder", Name: "Northwind Builder", Emoji: "🔨",
		Role:  RoleBuilder,
		Voice: "the worker: reads a card, changes code on a branch, opens a pull request",
	},
	{
		Slug: "northwind-reviewer", Name: "Northwind Reviewer", Emoji: "⚖️",
		Role:  RoleReviewer,
		Voice: "the verifier: reads a pull request diff and either signs it off or names one concrete thing to fix",
	},
}

// BySlug finds a member. It reports not-found rather than returning a zero
// Member, because an unattributed action is better than one attributed to a
// blank identity.
func BySlug(slug string) (Member, bool) {
	for _, m := range Roster {
		if m.Slug == slug {
			return m, true
		}
	}
	return Member{}, false
}

// ByEmail finds the member who sends from an address.
func ByEmail(email string) (Member, bool) {
	want := strings.ToLower(strings.TrimSpace(email))
	for _, m := range Roster {
		if m.Email != "" && strings.ToLower(m.Email) == want {
			return m, true
		}
	}
	return Member{}, false
}

// WithRole returns everyone holding a role.
func WithRole(role Role) []Member {
	var out []Member
	for _, m := range Roster {
		if m.Role == role {
			out = append(out, m)
		}
	}
	return out
}

// ============================ registration ============================

// Register creates every member in agent-store and tags it with its governed
// role in issues (issuesURL is bridgestores.IssuesURL; nothing calls Register
// today), and is safe to run repeatedly.
//
// Two stores because they hold two different things. agent-store owns the
// identity — the slug everything joins on, the display name, the voice. The
// governed role is a tag on that identity in kanban-store, which already
// supports tagging any (entity_type, entity_ref) pair and already registers
// `agent` as a type. Putting the role in agent-store's free-text `role` column
// instead would bury a controlled value inside prompt prose.
func Register(agentStoreURL, issuesURL string) error {
	existing, err := existingSlugs(agentStoreURL)
	if err != nil {
		return err
	}
	for _, m := range Roster {
		if !existing[m.Slug] {
			body := map[string]any{
				"slug": m.Slug, "display_name": m.Name, "emoji": m.Emoji,
				"projects": "northwind-api", "description": m.Voice,
				"role": m.Voice, "enabled": true,
			}
			if err := post(agentStoreURL+"/agents", body); err != nil {
				return fmt.Errorf("create agent %s: %w", m.Slug, err)
			}
		}
		tag := map[string]any{"tag": "role:" + string(m.Role)}
		url := fmt.Sprintf("%s/entities/agent/%s/tags", strings.TrimRight(issuesURL, "/"), m.Slug)
		if err := post(url, tag); err != nil {
			return fmt.Errorf("tag %s with %s: %w", m.Slug, m.Role, err)
		}
	}
	return nil
}

func existingSlugs(agentStoreURL string) (map[string]bool, error) {
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(strings.TrimRight(agentStoreURL, "/") + "/agents")
	if err != nil {
		return nil, fmt.Errorf("agent-store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent-store returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var agents []struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return nil, fmt.Errorf("decode agent list: %w", err)
	}
	out := make(map[string]bool, len(agents))
	for _, a := range agents {
		out[a.Slug] = true
	}
	return out, nil
}

func post(url string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// 409 is this call already having been made, which is the point of it being
	// repeatable.
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
