package main

// The session gate.
//
// A card on the Email board can be picked up by an agent, which links its
// bridge session to the card as an entity_type="session" link. While that agent
// is working, the classifier must not touch the card: it would retitle the work
// out from under the agent, retag it, move it between columns, rewrite the body
// the agent is reading, or — worst of all — archive it as a merge loser while a
// live session is still writing to it.
//
// So every classifier mutation asks this gate first. The gate resolves each
// linked session against llm-bridge-server once per tick and caches the answer,
// because one board view can carry the same session on several cards.
//
// What is deliberately NOT gated: attaching a new email link to a card. That is
// additive evidence and destroys nothing, so mail keeps filing onto a card even
// while an agent works on it. The rendered "## Linked emails" section in the
// body will lag behind those links until the session ends, which is safe in the
// one direction that matters — links are the source of truth and the body is a
// rendering of them, so the next tick after the session finishes re-renders it.
// Self-healing beats a stale body written over an agent's edits.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/slava/scheduler/internal/boundedtext"
	"github.com/slava/scheduler/internal/bridgesession"
	"github.com/slava/scheduler/internal/kanbanvocab"
)

// linkTypeSession is the entity_type that autoworker, kanban-dispatcher and the
// dash dispatch button all write when they hand a card to an agent.
const linkTypeSession = kanbanvocab.LinkTypeSession

// terminalSessionStates are the states that mean the agent is done and the card
// is the classifier's again. Taken from kanban-curator, which already owns this
// definition — it moves cards to Done on "completed" and to Failed on the other
// three. Keeping one list of terminal states across the two binaries is the
// point; a card the curator has already closed out must not still look busy here.
var terminalSessionStates = map[string]bool{
	"completed":    true,
	"error":        true,
	"aborted":      true,
	"disconnected": true,
}

// dormantSessionStates are alive but not working: the harness is up and the
// session could resume, yet nothing is running right now.
//
// These are windowed rather than trusted indefinitely. There are thousands of
// session links on this host and most of them are long-finished sessions parked
// in "idle", so treating dormancy as ongoing without a time bound would freeze
// those cards away from the classifier permanently.
var dormantSessionStates = map[string]bool{
	"idle":                true,
	"awaiting_permission": true,
	"awaiting_user":       true,
	"paused":              true,
}

// dormantSessionWindow is how long a dormant session still counts as owning its
// card. kanban-dispatcher uses the same hour to decide whether an idle session
// is still warm enough to reuse, and the two judgements should not disagree.
const dormantSessionWindow = time.Hour

// sessionGate answers "is an agent working on this card right now?".
type sessionGate struct {
	bridgeURL string
	client    *http.Client
	now       func() time.Time

	// ongoing caches one verdict per session id for the life of a tick.
	ongoing map[string]bool
}

func newSessionGate(bridgeURL string) *sessionGate {
	return &sessionGate{
		bridgeURL: bridgeURL,
		// A hung bridge must not hang the whole tick. Every call here is a
		// localhost GET, so a short timeout is generous.
		client:  &http.Client{Timeout: 5 * time.Second},
		now:     time.Now,
		ongoing: map[string]bool{},
	}
}

// agentOwns reports whether any session linked to this card is still ongoing,
// and names the session so the skip can be logged with something traceable.
func (g *sessionGate) agentOwns(card *cardState) (bool, string) {
	if g == nil || card == nil {
		return false, ""
	}
	for _, link := range card.AllLinks {
		if link.EntityType != linkTypeSession || link.EntityRef == "" {
			continue
		}
		if ongoing, why := g.sessionOngoing(link.EntityRef); ongoing {
			return true, fmt.Sprintf("%s (%s)", link.EntityRef, why)
		}
	}
	return false, ""
}

// sessionOngoing resolves one session against the bridge.
//
// An unresolvable session counts as ongoing. That is the fail-safe direction:
// if the bridge is unreachable we cannot tell whether an agent is mid-edit, and
// skipping a card costs one tick of filing latency, while guessing wrong costs
// an agent's work. Only cards that actually carry a session link are affected,
// so an unreachable bridge does not stall ordinary filing.
func (g *sessionGate) sessionOngoing(sessionID string) (bool, string) {
	if v, ok := g.ongoing[sessionID]; ok {
		return v, "already resolved this tick"
	}
	ongoing, why := g.resolve(sessionID)
	g.ongoing[sessionID] = ongoing
	return ongoing, why
}

func (g *sessionGate) resolve(sessionID string) (bool, string) {
	state, updatedAt, notFound, err := fetchSessionState(g.client, g.bridgeURL, sessionID)
	switch {
	case notFound:
		// The bridge has forgotten this session entirely. kanban-curator treats
		// that as reapable, so the card is free.
		return false, "session no longer exists"

	case err != nil:
		log.Printf("[email] WARNING: session %s unresolved (%v) — treating card as agent-owned and skipping it",
			sessionID, err)
		return true, "state unresolved"

	case terminalSessionStates[state]:
		return false, "state " + state

	case dormantSessionStates[state]:
		return g.dormantStillOwns(sessionID, state, updatedAt)

	case state == "":
		log.Printf("[email] WARNING: session %s returned an empty state — treating card as agent-owned and skipping it",
			sessionID)
		return true, "empty state"

	default:
		// Actively working: starting, model_generating, tool_running,
		// compacting, rate_limited, running. Anything unrecognised lands here
		// too, which is the safe side of an unknown state.
		return true, "state " + state
	}
}

// dormantStillOwns applies the freshness window to a dormant session.
func (g *sessionGate) dormantStillOwns(sessionID, state, updatedAt string) (bool, string) {
	if updatedAt == "" {
		log.Printf("[email] WARNING: session %s is %s with no updated_at — treating card as agent-owned and skipping it",
			sessionID, state)
		return true, state + ", age unknown"
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		// Both halves are bounded: updated_at comes from llm-bridge-server and
		// time.Parse quotes it back inside its own error, twice.
		log.Printf("[email] WARNING: session %s is %s with unparseable updated_at %q (%v) — treating card as agent-owned and skipping it",
			sessionID, state, boundedtext.Value(updatedAt), boundedtext.ErrorMessage(err))
		return true, state + ", age unparseable"
	}
	age := g.now().Sub(t)
	if age > dormantSessionWindow {
		return false, fmt.Sprintf("%s for %s", state, age.Round(time.Minute))
	}
	return true, fmt.Sprintf("%s for %s", state, age.Round(time.Minute))
}

// fetchSessionState reads one session from llm-bridge-server.
//
// It reports "not found" separately from an error on purpose. The two mean
// opposite things to the gate: a 404 frees the card, any other failure locks it.
// Collapsing them into one error — as the dispatcher's copy of this does, where
// the distinction does not matter — would make an unreachable bridge look
// exactly like a finished session and let the classifier edit live cards.
func fetchSessionState(client *http.Client, bridgeURL, sessionID string) (state, updatedAt string, notFound bool, err error) {
	// Built rather than interpolated, because an id that reaches a neighbouring
	// route answers 404 and a 404 is the one reply that frees the card. See
	// internal/bridgesession for the routes a bare fmt.Sprintf can hit.
	url, err := bridgesession.URL(bridgeURL, sessionID)
	if err != nil {
		return "", "", false, err
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", "", true, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", "", false, fmt.Errorf("HTTP %s — %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var s struct {
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return "", "", false, err
	}
	return s.State, s.UpdatedAt, false, nil
}
