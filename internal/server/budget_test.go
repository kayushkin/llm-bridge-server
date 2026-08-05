package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func float64Ptr(v float64) *float64 { return &v }

func TestCreateSession_StoresTheSpendCeiling(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Type:       msg.SessionTypeInteractive,
		Purpose:    msg.PurposeChat,
		Origin:     "test",
		Harness:    "claude_code",
		InstanceID: instID,
		MaxBudget:  float64Ptr(12.50),
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	sess := decodeJSON[msg.ManagedSession](t, resp)
	if sess.MaxBudgetUSD != 12.50 {
		t.Errorf("response max_budget_usd = %v; want 12.50", sess.MaxBudgetUSD)
	}

	// And it is persisted, not just echoed. A ceiling that only lives in
	// the response is a ceiling nothing enforces.
	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.MaxBudgetUSD != 12.50 {
		t.Errorf("stored max_budget_usd = %v; want 12.50", stored.MaxBudgetUSD)
	}
}

func TestCreateSession_AbsentCeilingMeansNoCeiling(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Type:       msg.SessionTypeInteractive,
		Purpose:    msg.PurposeChat,
		Origin:     "test",
		Harness:    "claude_code",
		InstanceID: instID,
	})
	sess := decodeJSON[msg.ManagedSession](t, resp)
	stored, err := st.GetSession(sess.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.MaxBudgetUSD != 0 {
		t.Errorf("stored max_budget_usd = %v; want 0 (no ceiling)", stored.MaxBudgetUSD)
	}
}

func TestCreateSession_RejectsNegativeCeiling(t *testing.T) {
	// A negative ceiling read as "unlimited" would turn an attempt to cap
	// spending into the absence of a cap. Rejected, never clamped.
	srv, _, instID := testServerWithInstance(t, "claude_code")

	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Type:       msg.SessionTypeInteractive,
		Purpose:    msg.PurposeChat,
		Origin:     "test",
		Harness:    "claude_code",
		InstanceID: instID,
		MaxBudget:  float64Ptr(-1),
	})
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400: %s", resp.StatusCode, body)
	}
}

func TestSendMessage_RefusedWhenTheSessionHasSpentItsCeiling(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	sess := &store.Session{
		SessionID:    "br_over_budget",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		MaxBudgetUSD: 5.00,
		SpendUSD:     5.25,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_over_budget/send", msg.SendMessageRequest{
		Message: "keep going",
	})
	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 402: %s", resp.StatusCode, body)
	}

	var payload struct {
		Error struct {
			Code         string  `json:"code"`
			SpendUSD     float64 `json:"spend_usd"`
			MaxBudgetUSD float64 `json:"max_budget_usd"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if payload.Error.Code != msg.ErrCodeBudgetExceeded {
		t.Errorf("error code = %q; want %q", payload.Error.Code, msg.ErrCodeBudgetExceeded)
	}
	if payload.Error.SpendUSD != 5.25 || payload.Error.MaxBudgetUSD != 5.00 {
		t.Errorf("refusal reported spend=%v ceiling=%v; want 5.25 / 5.00",
			payload.Error.SpendUSD, payload.Error.MaxBudgetUSD)
	}
}

func TestSendMessage_AllowedWhenUnderTheCeiling(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	if err := st.CreateSession(&store.Session{
		SessionID:    "br_under_budget",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		MaxBudgetUSD: 5.00,
		SpendUSD:     4.99,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_under_budget/send", msg.SendMessageRequest{
		Message: "keep going",
	})
	// The send fails further down for want of a real harness binary; what
	// matters here is that it was not refused by the budget gate.
	if resp.StatusCode == http.StatusPaymentRequired {
		t.Error("send refused as over budget at $4.99 of a $5.00 ceiling")
	}
}

func TestResumeSession_RefusedWhenTheSessionHasSpentItsCeiling(t *testing.T) {
	// Resuming is the other way to make a halted session spend again.
	srv, st, instID := testServerWithInstance(t, "claude_code")

	if err := st.CreateSession(&store.Session{
		SessionID:    "br_resume_over",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		MaxBudgetUSD: 1.00,
		SpendUSD:     2.00,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_resume_over/resume", nil)
	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 402: %s", resp.StatusCode, body)
	}
}

func TestConfigSession_RaisingTheCeilingUnblocksTheSession(t *testing.T) {
	// The escape hatch, end to end over HTTP: a halted session accepts a
	// new ceiling even though it has no live process to forward it to,
	// and the next send is let through.
	srv, st, instID := testServerWithInstance(t, "claude_code")

	if err := st.CreateSession(&store.Session{
		SessionID:    "br_raise_ceiling",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		MaxBudgetUSD: 1.00,
		SpendUSD:     1.50,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_raise_ceiling/send", msg.SendMessageRequest{Message: "hi"})
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("pre-raise send status = %d, want 402", resp.StatusCode)
	}

	resp = doJSON(t, srv, "POST", "/sessions/br_raise_ceiling/config", msg.ConfigSessionRequest{
		MaxBudget: float64Ptr(10.00),
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("raise-ceiling status = %d, want 200: %s", resp.StatusCode, body)
	}

	stored, err := st.GetSession("br_raise_ceiling")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.MaxBudgetUSD != 10.00 {
		t.Fatalf("stored ceiling = %v; want 10.00", stored.MaxBudgetUSD)
	}
	if stored.SpendUSD != 1.50 {
		t.Errorf("stored spend = %v; want it untouched at 1.50", stored.SpendUSD)
	}

	resp = doJSON(t, srv, "POST", "/sessions/br_raise_ceiling/send", msg.SendMessageRequest{Message: "hi"})
	if resp.StatusCode == http.StatusPaymentRequired {
		t.Error("send still refused after the ceiling was raised above the spend")
	}
}

func TestForkSession_InheritsTheParentsCeiling(t *testing.T) {
	// Without inheritance, forking is the way to get an uncapped session
	// out of a capped one — the whole gate defeated by one button.
	srv, st, instID := testServerWithInstance(t, "claude_code")

	if err := st.CreateSession(&store.Session{
		SessionID:        "br_fork_parent",
		HarnessSessionID: "cc-uuid-parent",
		Harness:          "claude_code",
		InstanceID:       instID,
		State:            string(msg.SessionIdle),
		MaxBudgetUSD:     7.00,
		SpendUSD:         6.90,
	}); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_fork_parent/fork", msg.ForkSessionRequest{})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("fork status = %d, want 201: %s", resp.StatusCode, body)
	}
	forked := decodeJSON[msg.ManagedSession](t, resp)

	stored, err := st.GetSession(forked.SessionID)
	if err != nil {
		t.Fatalf("get forked session: %v", err)
	}
	if stored.MaxBudgetUSD != 7.00 {
		t.Errorf("forked ceiling = %v; want the parent's 7.00", stored.MaxBudgetUSD)
	}
	if stored.SpendUSD != 0 {
		t.Errorf("forked spend = %v; want 0 — the fork starts its own tally", stored.SpendUSD)
	}
}

func TestStartOnInstance_RefusesAnOverBudgetSession(t *testing.T) {
	// The chokepoint check. Two spawn paths — auto-resume and the reaper's
	// revival of a killed session — have no HTTP handler in front of them,
	// so a guard that lived only in the handlers would let a halted
	// session come straight back. Auto-resume even replays the interrupted
	// turn, which is the turn the gate stopped for costing too much.
	srv, st, instID := testServerWithInstance(t, "claude_code")

	sess := &store.Session{
		SessionID:    "br_chokepoint",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		MaxBudgetUSD: 2.00,
		SpendUSD:     2.00,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	inst, err := srv.harnessStore.GetInstance(instID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}

	_, startErr := srv.startOnInstance(context.Background(), sess, inst, "")
	if startErr == nil {
		t.Fatal("startOnInstance spawned a process for a session that had spent its ceiling")
	}
	if !strings.Contains(startErr.Error(), "ceiling") {
		t.Errorf("error = %v; want it to say the ceiling was spent", startErr)
	}
}

func TestModeSwitch_RefusedWhenTheSessionHasSpentItsCeiling(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	if err := st.CreateSession(&store.Session{
		SessionID:    "br_mode_over",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		Mode:         msg.SessionModeEvents,
		MaxBudgetUSD: 1.00,
		SpendUSD:     4.00,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_mode_over/mode", map[string]string{"mode": "pty"})
	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 402: %s", resp.StatusCode, body)
	}
}

func TestConfigSession_RejectsNegativeCeiling(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, "claude_code")

	if err := st.CreateSession(&store.Session{
		SessionID:    "br_negative_config",
		Harness:      "claude_code",
		InstanceID:   instID,
		State:        string(msg.SessionIdle),
		MaxBudgetUSD: 5.00,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/br_negative_config/config", msg.ConfigSessionRequest{
		MaxBudget: float64Ptr(-5),
	})
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400: %s", resp.StatusCode, body)
	}

	stored, err := st.GetSession("br_negative_config")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.MaxBudgetUSD != 5.00 {
		t.Errorf("stored ceiling = %v after a rejected write; want it left at 5.00", stored.MaxBudgetUSD)
	}
}
