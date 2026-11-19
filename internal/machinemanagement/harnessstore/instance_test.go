package harnessstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestInstanceCRUD(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_host", "host")

	inst := &msg.Instance{
		ID:          "inst_cc",
		HarnessType: msg.HarnessClaudeCode,
		Name:        "cc-local",
		MachineID:   "m_host",
		WorkingDir:  "/work",
		Enabled:     true,
	}
	if err := s.CreateInstance(inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.CreatedAt.IsZero() || inst.UpdatedAt.IsZero() {
		t.Error("CreateInstance did not stamp timestamps")
	}
	// MaxConcurrentSessions defaults to 1.
	if inst.MaxConcurrentSessions != 1 {
		t.Errorf("MaxConcurrentSessions default = %d, want 1", inst.MaxConcurrentSessions)
	}

	got, err := s.GetInstance("inst_cc")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.HarnessType != msg.HarnessClaudeCode || got.Name != "cc-local" || got.MachineID != "m_host" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.WorkingDir != "/work" || !got.Enabled {
		t.Errorf("round-trip field mismatch: %+v", got)
	}
	if got.Machine != nil {
		t.Error("GetInstance should not populate Machine")
	}

	byName, err := s.GetInstanceByName("cc-local")
	if err != nil {
		t.Fatalf("GetInstanceByName: %v", err)
	}
	if byName.ID != "inst_cc" {
		t.Errorf("GetInstanceByName ID = %q, want inst_cc", byName.ID)
	}

	// Update.
	got.Name = "cc-renamed"
	got.MaxConcurrentSessions = 4
	got.Enabled = false
	if err := s.UpdateInstance(got); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	reloaded, err := s.GetInstance("inst_cc")
	if err != nil {
		t.Fatalf("GetInstance after update: %v", err)
	}
	if reloaded.Name != "cc-renamed" || reloaded.MaxConcurrentSessions != 4 || reloaded.Enabled {
		t.Errorf("update not persisted: %+v", reloaded)
	}

	if err := s.DeleteInstance("inst_cc"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := s.GetInstance("inst_cc"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstance after delete err = %v, want sql.ErrNoRows", err)
	}
}

func TestGetInstanceWithMachine(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_join", "joinhost")
	inst := &msg.Instance{ID: "inst_j", HarnessType: msg.HarnessCodex, Name: "codex", MachineID: "m_join", Enabled: true}
	if err := s.CreateInstance(inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	got, err := s.GetInstanceWithMachine("inst_j")
	if err != nil {
		t.Fatalf("GetInstanceWithMachine: %v", err)
	}
	if got.Machine == nil {
		t.Fatal("Machine not populated")
	}
	if got.Machine.ID != "m_join" || got.Machine.Name != "joinhost" {
		t.Errorf("joined Machine mismatch: %+v", got.Machine)
	}
}

func TestGetInstanceNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetInstance("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstance err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetInstanceByName("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstanceByName err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetInstanceWithMachine("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstanceWithMachine err = %v, want sql.ErrNoRows", err)
	}
}

func TestUpdateDeleteInstanceNotFound(t *testing.T) {
	s := newTestStore(t)
	ghost := &msg.Instance{ID: "ghost", HarnessType: msg.HarnessClaudeCode, Name: "ghost", MachineID: "m_x"}
	if err := s.UpdateInstance(ghost); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateInstance err = %v, want sql.ErrNoRows", err)
	}
	if err := s.DeleteInstance("ghost"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("DeleteInstance err = %v, want sql.ErrNoRows", err)
	}
	if err := s.SetInstanceEnabled("ghost", true); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetInstanceEnabled err = %v, want sql.ErrNoRows", err)
	}
}

func TestListInstancesAndFilters(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_1", "machine1")
	mustCreateMachine(t, s, "m_2", "machine2")

	insts := []*msg.Instance{
		{ID: "i_cc_a", HarnessType: msg.HarnessClaudeCode, Name: "cc-a", MachineID: "m_1", Enabled: true},
		{ID: "i_cc_b", HarnessType: msg.HarnessClaudeCode, Name: "cc-b", MachineID: "m_1", Enabled: false},
		{ID: "i_cx", HarnessType: msg.HarnessCodex, Name: "cx", MachineID: "m_2", Enabled: true},
	}
	for _, in := range insts {
		if err := s.CreateInstance(in); err != nil {
			t.Fatalf("CreateInstance %s: %v", in.ID, err)
		}
	}

	all, err := s.ListInstances()
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListInstances len = %d, want 3", len(all))
	}
	// Ordered by name: cc-a, cc-b, cx.
	if all[0].Name != "cc-a" || all[2].Name != "cx" {
		t.Errorf("ListInstances order: %q ... %q", all[0].Name, all[2].Name)
	}

	// ListInstancesByHarness returns only ENABLED instances of that harness.
	ccEnabled, err := s.ListInstancesByHarness(msg.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("ListInstancesByHarness: %v", err)
	}
	if len(ccEnabled) != 1 || ccEnabled[0].ID != "i_cc_a" {
		t.Errorf("ListInstancesByHarness = %+v, want only enabled i_cc_a", ccEnabled)
	}

	byMachine, err := s.ListInstancesByMachine("m_1")
	if err != nil {
		t.Fatalf("ListInstancesByMachine: %v", err)
	}
	if len(byMachine) != 2 {
		t.Errorf("ListInstancesByMachine(m_1) len = %d, want 2", len(byMachine))
	}
}

func TestInstanceCountsAndEnable(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_c", "counthost")

	if n, err := s.CountInstances(); err != nil || n != 0 {
		t.Fatalf("CountInstances empty = %d, %v", n, err)
	}

	for _, id := range []string{"a", "b", "c"} {
		in := &msg.Instance{ID: "i_" + id, HarnessType: msg.HarnessClaudeCode, Name: id, MachineID: "m_c", Enabled: true}
		if err := s.CreateInstance(in); err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
	}

	if n, err := s.CountInstances(); err != nil || n != 3 {
		t.Errorf("CountInstances = %d, %v; want 3", n, err)
	}
	if n, err := s.CountEnabledInstances(); err != nil || n != 3 {
		t.Errorf("CountEnabledInstances = %d, %v; want 3", n, err)
	}

	// Disable one and recount.
	if err := s.SetInstanceEnabled("i_b", false); err != nil {
		t.Fatalf("SetInstanceEnabled: %v", err)
	}
	if n, err := s.CountEnabledInstances(); err != nil || n != 2 {
		t.Errorf("CountEnabledInstances after disable = %d, %v; want 2", n, err)
	}

	got, err := s.GetInstance("i_b")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Enabled {
		t.Error("i_b still enabled after SetInstanceEnabled(false)")
	}
}
