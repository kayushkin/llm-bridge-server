package harnessstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestMachineCRUD(t *testing.T) {
	s := newTestStore(t)

	m := &msg.Machine{
		ID:                "m_linode",
		Name:              "linode",
		Emoji:             "☁",
		Hostname:          "linode.example",
		OS:                "linux",
		Arch:              "amd64",
		Transport:         msg.TransportSSH,
		SSHUser:           "deploy",
		SSHKeyPath:        "/home/deploy/.ssh/id_ed25519",
		SSHPort:           2222,
		DefaultWorkingDir: "/srv",
		User:              "deploy",
		Notes:             "primary remote",
	}
	if err := s.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Error("CreateMachine did not stamp CreatedAt/UpdatedAt")
	}

	got, err := s.GetMachine("m_linode")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.Name != "linode" || got.Transport != msg.TransportSSH || got.SSHPort != 2222 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Hostname != "linode.example" || got.SSHUser != "deploy" || got.Notes != "primary remote" {
		t.Errorf("round-trip field mismatch: %+v", got)
	}

	byName, err := s.GetMachineByName("linode")
	if err != nil {
		t.Fatalf("GetMachineByName: %v", err)
	}
	if byName.ID != "m_linode" {
		t.Errorf("GetMachineByName ID = %q, want m_linode", byName.ID)
	}

	// Update mutable fields.
	got.Notes = "updated note"
	got.SSHPort = 22
	if err := s.UpdateMachine(got); err != nil {
		t.Fatalf("UpdateMachine: %v", err)
	}
	reloaded, err := s.GetMachine("m_linode")
	if err != nil {
		t.Fatalf("GetMachine after update: %v", err)
	}
	if reloaded.Notes != "updated note" || reloaded.SSHPort != 22 {
		t.Errorf("update not persisted: %+v", reloaded)
	}

	if err := s.DeleteMachine("m_linode"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	if _, err := s.GetMachine("m_linode"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetMachine after delete err = %v, want sql.ErrNoRows", err)
	}
}

func TestCreateMachineDefaults(t *testing.T) {
	s := newTestStore(t)

	// Empty transport and zero SSH port should be defaulted on create.
	m := &msg.Machine{ID: "m_def", Name: "defaults"}
	if err := s.CreateMachine(m); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if m.SSHPort != 22 {
		t.Errorf("SSHPort default = %d, want 22", m.SSHPort)
	}
	if m.Transport != msg.TransportLocal {
		t.Errorf("Transport default = %q, want %q", m.Transport, msg.TransportLocal)
	}

	got, err := s.GetMachine("m_def")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.SSHPort != 22 || got.Transport != msg.TransportLocal {
		t.Errorf("defaults not persisted: %+v", got)
	}
}

func TestGetMachineNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetMachine("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetMachine err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetMachineByName("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetMachineByName err = %v, want sql.ErrNoRows", err)
	}
}

func TestUpdateDeleteMachineNotFound(t *testing.T) {
	s := newTestStore(t)
	ghost := &msg.Machine{ID: "ghost", Name: "ghost", Transport: msg.TransportLocal}
	if err := s.UpdateMachine(ghost); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateMachine err = %v, want sql.ErrNoRows", err)
	}
	if err := s.DeleteMachine("ghost"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("DeleteMachine err = %v, want sql.ErrNoRows", err)
	}
}

func TestListMachines(t *testing.T) {
	s := newTestStore(t)
	if ms, err := s.ListMachines(); err != nil {
		t.Fatalf("ListMachines empty: %v", err)
	} else if len(ms) != 0 {
		t.Errorf("len = %d, want 0", len(ms))
	}

	mustCreateMachine(t, s, "m_b", "bravo")
	mustCreateMachine(t, s, "m_a", "alpha")
	mustCreateMachine(t, s, "m_c", "charlie")

	ms, err := s.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("len = %d, want 3", len(ms))
	}
	// Ordered by name.
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if ms[i].Name != w {
			t.Errorf("ms[%d].Name = %q, want %q", i, ms[i].Name, w)
		}
	}
}

func TestMachineRunnerTokenHash(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_run", "runner1")

	token, err := GenerateRunnerToken()
	if err != nil {
		t.Fatalf("GenerateRunnerToken: %v", err)
	}
	hash := HashRunnerToken(token)

	if err := s.SetMachineRunnerTokenHash("m_run", hash); err != nil {
		t.Fatalf("SetMachineRunnerTokenHash: %v", err)
	}

	got, err := s.GetMachineByRunnerTokenHash(hash)
	if err != nil {
		t.Fatalf("GetMachineByRunnerTokenHash: %v", err)
	}
	if got.ID != "m_run" {
		t.Errorf("ID = %q, want m_run", got.ID)
	}

	// Empty hash short-circuits to ErrNoRows without a query.
	if _, err := s.GetMachineByRunnerTokenHash(""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty-hash lookup err = %v, want sql.ErrNoRows", err)
	}
	// Unknown hash also misses.
	if _, err := s.GetMachineByRunnerTokenHash("deadbeef"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown-hash lookup err = %v, want sql.ErrNoRows", err)
	}

	// Setting a token hash on a missing machine is a no-op miss.
	if err := s.SetMachineRunnerTokenHash("ghost", hash); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetMachineRunnerTokenHash(ghost) err = %v, want sql.ErrNoRows", err)
	}
}

func TestTouchMachineLastSeen(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_seen", "seen")

	before, err := s.GetMachine("m_seen")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if !before.LastSeenAt.IsZero() {
		t.Errorf("LastSeenAt should start zero, got %v", before.LastSeenAt)
	}

	if err := s.TouchMachineLastSeen("m_seen"); err != nil {
		t.Fatalf("TouchMachineLastSeen: %v", err)
	}
	after, err := s.GetMachine("m_seen")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if after.LastSeenAt.IsZero() {
		t.Error("LastSeenAt still zero after touch")
	}
}

func TestGenerateRunnerTokenUnique(t *testing.T) {
	a, err := GenerateRunnerToken()
	if err != nil {
		t.Fatalf("GenerateRunnerToken: %v", err)
	}
	b, err := GenerateRunnerToken()
	if err != nil {
		t.Fatalf("GenerateRunnerToken: %v", err)
	}
	if a == b {
		t.Error("two runner tokens collided")
	}
	if len(a) != 64 { // 32 bytes hex-encoded
		t.Errorf("token len = %d, want 64", len(a))
	}
	// Hashing is deterministic.
	if HashRunnerToken(a) != HashRunnerToken(a) {
		t.Error("HashRunnerToken not deterministic")
	}
}

func TestComputerUseIsOffUntilTheOwnerSwitchesItOnAndEditsDoNotTouchIt(t *testing.T) {
	s := newTestStore(t)
	m := &msg.Machine{ID: "m_cu", Name: "cu-box", Transport: msg.TransportRunner}
	if err := s.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMachine("m_cu")
	if err != nil {
		t.Fatal(err)
	}
	if got.ComputerUseAllowed {
		t.Fatal("a new machine allows computer use")
	}
	if err := s.SetMachineComputerUseAllowed("m_cu", true); err != nil {
		t.Fatal(err)
	}
	// An edit carrying a stale false must not switch it back off.
	got.Notes = "renamed"
	got.ComputerUseAllowed = false
	if err := s.UpdateMachine(got); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.GetMachine("m_cu"); !after.ComputerUseAllowed || after.Notes != "renamed" {
		t.Errorf("after an edit: allowed=%v notes=%q; want true, renamed", after.ComputerUseAllowed, after.Notes)
	}
	if err := s.SetMachineComputerUseAllowed("m_missing", true); err == nil {
		t.Error("switching a machine that does not exist succeeded")
	}
}
