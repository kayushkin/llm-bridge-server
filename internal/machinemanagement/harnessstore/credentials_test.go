package harnessstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// setupInstance creates a machine + instance so credential bindings have a
// real instance_id to reference.
func setupInstance(t *testing.T, s *Store, instID string) {
	t.Helper()
	mustCreateMachine(t, s, "m_cred", "credhost")
	inst := &msg.Instance{ID: instID, HarnessType: msg.HarnessClaudeCode, Name: instID, MachineID: "m_cred", Enabled: true}
	if err := s.CreateInstance(inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
}

func TestBindAndGetCredential(t *testing.T) {
	s := newTestStore(t)
	setupInstance(t, s, "inst_1")

	ic := &msg.InstanceCredential{InstanceID: "inst_1", CredentialID: "cred_a", Priority: 0, Enabled: true}
	if err := s.BindCredential(ic); err != nil {
		t.Fatalf("BindCredential: %v", err)
	}

	got, err := s.GetCredentialBinding("inst_1", "cred_a")
	if err != nil {
		t.Fatalf("GetCredentialBinding: %v", err)
	}
	if got.Priority != 0 || !got.Enabled {
		t.Errorf("binding mismatch: %+v", got)
	}

	// BindCredential is upsert — re-binding updates priority/enabled.
	ic.Priority = 5
	ic.Enabled = false
	if err := s.BindCredential(ic); err != nil {
		t.Fatalf("re-BindCredential: %v", err)
	}
	got, err = s.GetCredentialBinding("inst_1", "cred_a")
	if err != nil {
		t.Fatalf("GetCredentialBinding: %v", err)
	}
	if got.Priority != 5 || got.Enabled {
		t.Errorf("upsert did not update: %+v", got)
	}

	if n, err := s.CountCredentialBindings("inst_1"); err != nil || n != 1 {
		t.Errorf("CountCredentialBindings = %d, %v; want 1 (upsert, not duplicate)", n, err)
	}
}

func TestGetCredentialBindingNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetCredentialBinding("inst_x", "cred_x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestListInstanceCredentialsOrdered(t *testing.T) {
	s := newTestStore(t)
	setupInstance(t, s, "inst_1")

	// Bind out of priority order; list should come back sorted by priority.
	binds := []*msg.InstanceCredential{
		{InstanceID: "inst_1", CredentialID: "cred_hi", Priority: 2, Enabled: true},
		{InstanceID: "inst_1", CredentialID: "cred_lo", Priority: 0, Enabled: true},
		{InstanceID: "inst_1", CredentialID: "cred_mid", Priority: 1, Enabled: false},
	}
	for _, b := range binds {
		if err := s.BindCredential(b); err != nil {
			t.Fatalf("BindCredential %s: %v", b.CredentialID, err)
		}
	}

	got, err := s.ListInstanceCredentials("inst_1")
	if err != nil {
		t.Fatalf("ListInstanceCredentials: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	wantOrder := []string{"cred_lo", "cred_mid", "cred_hi"}
	for i, w := range wantOrder {
		if got[i].CredentialID != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i].CredentialID, w)
		}
	}
	// Enabled bool decoded correctly.
	if !got[0].Enabled || got[1].Enabled {
		t.Errorf("enabled decode mismatch: %+v", got)
	}

	// Empty instance → empty slice, no error.
	empty, err := s.ListInstanceCredentials("inst_none")
	if err != nil {
		t.Fatalf("ListInstanceCredentials empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("len = %d, want 0", len(empty))
	}
}

func TestListCredentialInstances(t *testing.T) {
	s := newTestStore(t)
	// Two instances sharing one credential.
	mustCreateMachine(t, s, "m_cred", "credhost")
	for _, id := range []string{"inst_a", "inst_b"} {
		inst := &msg.Instance{ID: id, HarnessType: msg.HarnessClaudeCode, Name: id, MachineID: "m_cred", Enabled: true}
		if err := s.CreateInstance(inst); err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
		if err := s.BindCredential(&msg.InstanceCredential{InstanceID: id, CredentialID: "shared", Enabled: true}); err != nil {
			t.Fatalf("BindCredential: %v", err)
		}
	}

	got, err := s.ListCredentialInstances("shared")
	if err != nil {
		t.Fatalf("ListCredentialInstances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Ordered by instance_id.
	if got[0].InstanceID != "inst_a" || got[1].InstanceID != "inst_b" {
		t.Errorf("order = %q, %q; want inst_a, inst_b", got[0].InstanceID, got[1].InstanceID)
	}
}

func TestSetCredentialEnabledAndPriority(t *testing.T) {
	s := newTestStore(t)
	setupInstance(t, s, "inst_1")
	if err := s.BindCredential(&msg.InstanceCredential{InstanceID: "inst_1", CredentialID: "cred_a", Priority: 0, Enabled: true}); err != nil {
		t.Fatalf("BindCredential: %v", err)
	}

	if err := s.SetCredentialEnabled("inst_1", "cred_a", false); err != nil {
		t.Fatalf("SetCredentialEnabled: %v", err)
	}
	if err := s.UpdateCredentialPriority("inst_1", "cred_a", 9); err != nil {
		t.Fatalf("UpdateCredentialPriority: %v", err)
	}

	got, err := s.GetCredentialBinding("inst_1", "cred_a")
	if err != nil {
		t.Fatalf("GetCredentialBinding: %v", err)
	}
	if got.Enabled || got.Priority != 9 {
		t.Errorf("updates not persisted: %+v", got)
	}

	// Both update methods miss on an unknown binding.
	if err := s.SetCredentialEnabled("inst_1", "ghost", true); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetCredentialEnabled miss err = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateCredentialPriority("inst_1", "ghost", 1); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateCredentialPriority miss err = %v, want sql.ErrNoRows", err)
	}
}

func TestUnbindAndClearCredentials(t *testing.T) {
	s := newTestStore(t)
	setupInstance(t, s, "inst_1")
	for _, c := range []string{"c1", "c2", "c3"} {
		if err := s.BindCredential(&msg.InstanceCredential{InstanceID: "inst_1", CredentialID: c, Enabled: true}); err != nil {
			t.Fatalf("BindCredential %s: %v", c, err)
		}
	}

	if err := s.UnbindCredential("inst_1", "c2"); err != nil {
		t.Fatalf("UnbindCredential: %v", err)
	}
	if n, err := s.CountCredentialBindings("inst_1"); err != nil || n != 2 {
		t.Errorf("count after unbind = %d, %v; want 2", n, err)
	}
	// Unbinding a missing binding reports sql.ErrNoRows.
	if err := s.UnbindCredential("inst_1", "c2"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UnbindCredential miss err = %v, want sql.ErrNoRows", err)
	}

	if err := s.ClearInstanceCredentials("inst_1"); err != nil {
		t.Fatalf("ClearInstanceCredentials: %v", err)
	}
	if n, err := s.CountCredentialBindings("inst_1"); err != nil || n != 0 {
		t.Errorf("count after clear = %d, %v; want 0", n, err)
	}
	// Clearing an already-empty instance is a no-op, not an error.
	if err := s.ClearInstanceCredentials("inst_1"); err != nil {
		t.Errorf("ClearInstanceCredentials on empty: %v", err)
	}
}
