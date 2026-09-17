// Package demosession runs a one-shot task as a harness session on the
// instance the caller names.
//
// The reason it is a session and not a model call is billing, and it is not a
// detail. A direct call to /v1/messages spends API credit; a Claude Code
// session runs on the Max subscription, where the marginal cost of a turn is
// zero. Everything on this host that dispatches agents already works that way.
//
// Which instance hosts it is not this package's to decide: the kanban board the
// work came from owns that as default_instance_id, and the caller reads it
// there. The harness type is then whatever llm-bridge-server says that
// instance runs — read at spawn, never assumed, so a board pointed at a codex
// instance does not get a claude_code request it would refuse.
//
// It talks to llm-bridge-server over HTTP rather than publishing to NATS, which
// is what autoworker and kanban-dispatcher do. Two reasons, both learned here:
//
//   - A NATS publish is fire-and-forget. It succeeds whether or not a session
//     is ever created, so a caller that publishes and then links the session id
//     to a card can leave the card pointing at a session that never existed.
//     That happened: the first dispatch produced a session that did nothing,
//     went idle having spent nothing, and reported no error anywhere.
//   - The NATS route passes through llm-bridge-adapter, which for three months
//     silently dropped every CreateSessionRequest field added after the binary
//     was built — working_dir and max_budget among them — because it decoded
//     into a compiled-in struct. Going straight to the server removes the hop.
//
// The session id is still minted by the caller, so a card's session link
// resolves exactly as it did over NATS: the server only invents one when the
// request leaves it empty.
package demosession

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/slava/scheduler/internal/bridgeauth"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/slava/scheduler/internal/bridgeclient"
)

// Task is one piece of work to hand an agent.
type Task struct {
	// Prompt is the whole instruction. It must describe work with a visible
	// result, because nothing reads a value back out of the session.
	Prompt string
	// AgentSlug is the identity the session acts as, written to
	// CreateSessionRequest.AgentID.
	AgentSlug string
	// DisplayName is what a human sees in the session list.
	DisplayName string
	// WorkingDir is where the harness runs. Empty leaves it to the instance.
	WorkingDir string
	// MaxBudgetUSD caps the session. Zero means uncapped, which is what a
	// harness reads a missing flag as — so a caller that wants a cap must say a
	// positive number.
	MaxBudgetUSD float64
	// InstanceID is the harness instance to host it: the board's
	// default_instance_id. Required — there is no fallback instance, because
	// the one that used to be written here ("inst-cc-local") was a second copy
	// of a board setting and the two would drift.
	InstanceID string
	// BundleID is bundle-store's numeric id as a string, sent verbatim as
	// bundle_id; llm-bridge-server checks it with bundle-store and provisions
	// the bundle's tools at spawn. Empty sends no bundle.
	BundleID string
	// Purpose and Origin are governed vocabularies on llm-bridge-server and are
	// enforced at creation, so they are not free text.
	Purpose string
	Origin  string
}

// Spawn creates the session, starts it, and sends it the prompt.
//
// It returns the session id only once the harness is actually running. A
// caller can therefore link the id to a card knowing it refers to something.
func Spawn(bridgeURL string, t Task) (string, error) {
	if t.Prompt == "" {
		return "", fmt.Errorf("demosession: a task with no prompt would spawn an agent with nothing to do")
	}
	if t.AgentSlug == "" {
		return "", fmt.Errorf("demosession: a session with no identity cannot be attributed to anyone")
	}
	if bridgeURL == "" {
		return "", fmt.Errorf("demosession: no llm-bridge-server URL")
	}
	if t.InstanceID == "" {
		return "", fmt.Errorf("demosession: no instance id — the board's default_instance_id is unset, and there is no fallback instance")
	}
	if t.Purpose == "" {
		t.Purpose = "autoworker"
	}
	if t.Origin == "" {
		t.Origin = "scheduler"
	}

	harness, err := bridgeclient.InstanceHarnessType(bridgeURL, t.InstanceID)
	if err != nil {
		return "", fmt.Errorf("demosession: %w", err)
	}

	now := time.Now().UTC()
	sessionID := fmt.Sprintf("demo-%s-%d", t.AgentSlug, now.UnixNano())

	create := msg.CreateSessionRequest{
		SessionID:   sessionID,
		Harness:     harness,
		InstanceID:  t.InstanceID,
		BundleID:    t.BundleID,
		DisplayName: t.DisplayName,
		Purpose:     t.Purpose,
		Origin:      t.Origin,
		Type:        msg.SessionTypeAutonomous,
		AutoStart:   true,
		AgentID:     t.AgentSlug,
		WorkingDir:  t.WorkingDir,
	}
	if t.MaxBudgetUSD > 0 {
		budget := t.MaxBudgetUSD
		create.MaxBudget = &budget
		// The harness-side cap as well as the server-side one. A poller that
		// notices a breach afterwards is not a control: the fastest session
		// observed on this host burned $29.22 a minute.
		create.HarnessConfig = []byte(fmt.Sprintf(`{"max_budget_usd":%.2f}`, budget))
	}

	base := strings.TrimRight(bridgeURL, "/")
	var created struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	}
	if err := postJSON(base+"/sessions", create, &created); err != nil {
		return "", fmt.Errorf("demosession: create session: %w", err)
	}
	// ⚠️ Creation answers 201 even when the harness failed to start — the
	// server records the failure as the session's state and returns it. Reading
	// only the status code would report a working agent that does not exist.
	if created.State == string(msg.SessionError) {
		return "", fmt.Errorf("demosession: session %s was created but its harness failed to start", sessionID)
	}
	if created.SessionID != "" && created.SessionID != sessionID {
		// The caller mints the id so a card's link resolves to it. A server that
		// chose a different one would leave that link pointing nowhere.
		return "", fmt.Errorf("demosession: asked for session %q and the server made %q", sessionID, created.SessionID)
	}

	if err := postJSON(base+"/sessions/"+sessionID+"/send",
		msg.SendMessageRequest{Message: t.Prompt}, nil); err != nil {
		return "", fmt.Errorf("demosession: session %s started but the prompt did not reach it: %w", sessionID, err)
	}
	return sessionID, nil
}

// postJSON sends one request and insists on a 2xx, carrying the body on
// failure. A non-2xx swallowed here is a session nobody knows did not start.
func postJSON(url string, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := bridgeauth.AuthenticateClient(&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode reply from %s: %w", url, err)
		}
	}
	return nil
}
