package server

import (
	"encoding/json"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// The wire types below mirror chat-core/src/net/types.ts EXACTLY (camelCase
// field names, RFC3339+offset timestamps). See docs/WIRE.md — that contract is
// authoritative and these structs must serialize to it. All dashv2 endpoints
// are additive; nothing here changes an existing endpoint's shape.

// SessionSummary is the projected sidebar row. Deliberately omits the heavy
// info / harness_config blobs.
type SessionSummary struct {
	SessionID   string `json:"sessionId"`
	State       string `json:"state"`
	Harness     string `json:"harness"`
	InstanceID  string `json:"instanceId"`
	Type        string `json:"type"`
	Purpose     string `json:"purpose"`
	Mode        string `json:"mode"`
	FolderName  string `json:"folderName"`
	DisplayName string `json:"displayName"`
	AgentID     string `json:"agentId"`
	// ManagerSessionID is the bridge session id of the session that spawned this
	// one; empty for a top-level session. Named exactly as chat-core's
	// SessionSummary already declares it (`managerSessionId`), which the SSE
	// upsert path has always carried — this list is what was missing it.
	ManagerSessionID string `json:"managerSessionId"`
	UpdatedAt        string `json:"updatedAt"`
	CreatedAt        string `json:"createdAt"`
}

// SummaryResponse is the /sessions/summary wire shape. Next is null when there
// are no more pages; Revision doubles as the ETag.
type SummaryResponse struct {
	Sessions []SessionSummary `json:"sessions"`
	Next     *string          `json:"next"`
	Revision string           `json:"revision"`
}

// Validator is the cheap staleness currency (matches types.ts Validator).
type Validator struct {
	MaxEventID int64  `json:"maxEventId"`
	EventCount int    `json:"eventCount"`
	UpdatedAt  string `json:"updatedAt"`
}

// bundleEntry is one session's slot in RecentBundleResponse: its summary plus
// its materialized turn model. Model is carried as raw JSON — the TurnModel
// shape is owned by log-store (which produced it), so bridge-server passes it
// through unchanged rather than re-declaring the type (layers are transparent).
type bundleEntry struct {
	Summary SessionSummary  `json:"summary"`
	Model   json.RawMessage `json:"model"`

	// Stream is where to resume this session's event stream so that the bundled model
	// and the stream meet exactly — the same fact `SessionMessagesResponse` carries, for
	// the same reason. Without it a bundle-warmed session is opened with no resume
	// point, the click path skips its fetch because the model is already in the store,
	// and the server replays the entire current turn at a client that has all of it.
	// That is most sessions on a cold boot, so omitting it here would leave the whole
	// mechanism unreachable for exactly the sessions people open first.
	Stream StreamResumePoint `json:"stream"`
}

// summaryFromRow formats a projected store row into the wire SessionSummary,
// rendering timestamps as RFC3339 with offset.
func summaryFromRow(r store.SessionSummaryRow) SessionSummary {
	return SessionSummary{
		SessionID:   r.SessionID,
		State:       r.State,
		Harness:     r.Harness,
		InstanceID:  r.InstanceID,
		Type:        r.Type,
		Purpose:     r.Purpose,
		Mode:        r.Mode,
		FolderName:  r.FolderName,
		DisplayName: r.DisplayName,
		AgentID:     r.AgentID,
		UpdatedAt:   formatWireTime(r.UpdatedAt),
		CreatedAt:   formatWireTime(r.CreatedAt),

		ManagerSessionID: r.ManagerSessionID,
	}
}

// formatWireTime renders a time as RFC3339 with offset, never naive. Zero time
// yields "".
func formatWireTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
