package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// seedSubagentParent creates a parent session for the promotion tests.
func seedSubagentParent(t *testing.T, s *Store, id string) *Session {
	t.Helper()
	parent := &Session{
		SessionID:  id,
		Harness:    msg.HarnessClaudeCode,
		InstanceID: "inst-cc-local",
		State:      string(msg.SessionRunning),
		Purpose:    "chat",
		Type:       msg.SessionTypeInteractive,
		FolderName: "Work",
	}
	if err := s.CreateSession(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	return parent
}

func TestEnsureSubagentSessionLinksToParent(t *testing.T) {
	s := testStore(t)
	parent := seedSubagentParent(t, s, "br_parent")

	id, created, err := s.EnsureSubagentSession(parent, "agent-abc123", "Map a reference store service", "Subagents")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !created {
		t.Fatal("first call reported the session already existed")
	}

	sub, err := s.GetSession(id)
	if err != nil {
		t.Fatalf("get subagent: %v", err)
	}
	if sub.ManagerSessionID != parent.SessionID {
		t.Errorf("manager_session_id = %q, want %q", sub.ManagerSessionID, parent.SessionID)
	}
	if sub.RootSessionID != parent.SessionID {
		t.Errorf("root_session_id = %q, want %q — a top-level parent is its own tree root", sub.RootSessionID, parent.SessionID)
	}
	if sub.Depth != 1 {
		t.Errorf("depth = %d, want 1", sub.Depth)
	}
	if sub.ControlledBy != "harness" {
		t.Errorf("controlled_by = %q, want harness", sub.ControlledBy)
	}
	if sub.Purpose != "subagent" {
		t.Errorf("purpose = %q, want subagent", sub.Purpose)
	}
	if sub.Type != msg.SessionTypeSystem {
		t.Errorf("type = %q, want system", sub.Type)
	}
	if sub.Harness != parent.Harness {
		t.Errorf("harness = %q, want %q", sub.Harness, parent.Harness)
	}
	if sub.InstanceID != parent.InstanceID {
		t.Errorf("instance_id = %q, want %q", sub.InstanceID, parent.InstanceID)
	}
	if sub.FolderName != "Subagents" {
		t.Errorf("folder_name = %q, want Subagents", sub.FolderName)
	}
}

// TestEnsureSubagentSessionNestsUnderASubagent covers a subagent that spawns
// its own subagent: the tree deepens but the root stays put, which is what
// makes "show me this whole team" a single query.
func TestEnsureSubagentSessionNestsUnderASubagent(t *testing.T) {
	s := testStore(t)
	root := seedSubagentParent(t, s, "br_root")

	l1ID, _, err := s.EnsureSubagentSession(root, "agent-l1", "level one", "")
	if err != nil {
		t.Fatalf("ensure L1: %v", err)
	}
	l1, err := s.GetSession(l1ID)
	if err != nil {
		t.Fatalf("get L1: %v", err)
	}

	l2ID, _, err := s.EnsureSubagentSession(l1, "agent-l2", "level two", "")
	if err != nil {
		t.Fatalf("ensure L2: %v", err)
	}
	l2, err := s.GetSession(l2ID)
	if err != nil {
		t.Fatalf("get L2: %v", err)
	}

	if l2.ManagerSessionID != l1ID {
		t.Errorf("L2 manager_session_id = %q, want %q", l2.ManagerSessionID, l1ID)
	}
	if l2.RootSessionID != root.SessionID {
		t.Errorf("L2 root_session_id = %q, want %q — root must stay the top of the tree, not the immediate parent", l2.RootSessionID, root.SessionID)
	}
	if l2.Depth != 2 {
		t.Errorf("L2 depth = %d, want 2", l2.Depth)
	}
}

func TestEnsureSubagentSessionIsIdempotent(t *testing.T) {
	s := testStore(t)
	parent := seedSubagentParent(t, s, "br_parent")

	first, created, err := s.EnsureSubagentSession(parent, "agent-abc123", "probe", "")
	if err != nil || !created {
		t.Fatalf("first ensure: id=%q created=%v err=%v", first, created, err)
	}
	second, created, err := s.EnsureSubagentSession(parent, "agent-abc123", "probe", "")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if created {
		t.Error("second ensure reported a fresh create")
	}
	if second != first {
		t.Fatalf("second ensure minted a new session %q; want %q", second, first)
	}

	sessions, err := s.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (parent + one subagent)", len(sessions))
	}
}

// TestEnsureSubagentSessionConvergesWithDiscovery is the claim the whole
// dedupe-key choice rests on. The live demux promotes a subagent while the run
// is in flight; the rollout scanner later walks the same subagent's
// subagents/agent-<task_id>.jsonl file. Both must land on ONE row. If they
// diverge, every subagent is recorded twice — once linked, once orphaned.
func TestEnsureSubagentSessionConvergesWithDiscovery(t *testing.T) {
	s := testStore(t)
	parent := seedSubagentParent(t, s, "br_parent")

	const harnessSessionID = "agent-acfc3596055b9d899" // from subagents/agent-<task_id>.jsonl

	liveID, _, err := s.EnsureSubagentSession(parent, harnessSessionID, "Map a reference store service", "Subagents")
	if err != nil {
		t.Fatalf("live promote: %v", err)
	}

	// What the discovery scanner does when it later finds the rollout file.
	now := time.Now().UTC()
	discoveredID, inserted, err := s.UpsertDiscoveredSession(
		harnessSessionID, "", "Map a reference store service", string(msg.HarnessClaudeCode),
		"inst-cc-local", "subagent", "Subagents", now, now)
	if err != nil {
		t.Fatalf("discovery upsert: %v", err)
	}
	if inserted {
		t.Error("discovery inserted a second row for a subagent the demux had already promoted")
	}
	if discoveredID != liveID {
		t.Fatalf("discovery resolved to %q, live demux to %q; the two paths diverged", discoveredID, liveID)
	}

	// And the lineage survives the discovery upsert — it must not blank the link.
	sub, err := s.GetSession(liveID)
	if err != nil {
		t.Fatalf("get subagent: %v", err)
	}
	if sub.ManagerSessionID != parent.SessionID {
		t.Fatalf("discovery upsert dropped manager_session_id (now %q)", sub.ManagerSessionID)
	}
}

func TestEnsureSubagentSessionRejectsUnusableKeys(t *testing.T) {
	s := testStore(t)
	parent := seedSubagentParent(t, s, "br_parent")

	if _, _, err := s.EnsureSubagentSession(parent, "", "probe", ""); err == nil {
		t.Error("empty harness_session_id accepted; it would collapse every subagent onto one dedupe key")
	}
	if _, _, err := s.EnsureSubagentSession(parent, "br_1234", "probe", ""); err == nil {
		t.Error("bridge id accepted in the harness slot")
	}
	if _, _, err := s.EnsureSubagentSession(nil, "agent-x", "probe", ""); err == nil {
		t.Error("nil parent accepted")
	}
}

// TestLinkDiscoveredSessionParent covers the cold-import lineage link and the
// three things it must refuse to do. Before it existed, every subagent that
// reached the store through discovery rather than the live demux landed with no
// parent — 1,259 such rows on the host this was written on.
func TestLinkDiscoveredSessionParent(t *testing.T) {
	newStore := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		return s
	}
	now := time.Now()

	mkParent := func(t *testing.T, s *Store, harnessID string) string {
		t.Helper()
		id, _, err := s.UpsertDiscoveredSession(harnessID, "", "parent", "claude_code", "", "", "", now, now)
		if err != nil {
			t.Fatalf("seed parent: %v", err)
		}
		return id
	}
	mkChild := func(t *testing.T, s *Store, harnessID string) string {
		t.Helper()
		id, _, err := s.UpsertDiscoveredSession(harnessID, "", "child", "claude_code", "", "subagent", "Subagents", now, now)
		if err != nil {
			t.Fatalf("seed child: %v", err)
		}
		return id
	}

	t.Run("links a discovered subagent to its parent", func(t *testing.T) {
		s := newStore(t)
		parentID := mkParent(t, s, "parent-uuid")
		childID := mkChild(t, s, "agent-a369d3bc")

		linked, err := s.LinkDiscoveredSessionParent(childID, "parent-uuid")
		if err != nil || !linked {
			t.Fatalf("linked = %v, err = %v; want true, nil", linked, err)
		}
		child, err := s.GetSession(childID)
		if err != nil {
			t.Fatalf("get child: %v", err)
		}
		if child.ManagerSessionID != parentID {
			t.Errorf("manager_session_id = %q, want %q (the parent's BRIDGE id, not its harness id)", child.ManagerSessionID, parentID)
		}
		if child.RootSessionID != parentID {
			t.Errorf("root_session_id = %q, want %q", child.RootSessionID, parentID)
		}
		if child.Depth != 1 {
			t.Errorf("depth = %d, want 1", child.Depth)
		}
	})

	t.Run("is idempotent, so every discovery pass may run it", func(t *testing.T) {
		s := newStore(t)
		mkParent(t, s, "parent-uuid")
		childID := mkChild(t, s, "agent-a369d3bc")

		if _, err := s.LinkDiscoveredSessionParent(childID, "parent-uuid"); err != nil {
			t.Fatalf("first link: %v", err)
		}
		linked, err := s.LinkDiscoveredSessionParent(childID, "parent-uuid")
		if err != nil {
			t.Fatalf("second link: %v", err)
		}
		if linked {
			t.Error("reported a second write; the link already existed and must not be rewritten")
		}
	})

	t.Run("does not overwrite a link the live path already wrote", func(t *testing.T) {
		s := newStore(t)
		mkParent(t, s, "parent-uuid")
		realParent := mkParent(t, s, "the-real-parent")
		childID := mkChild(t, s, "agent-a369d3bc")

		if _, err := s.db.Exec(`UPDATE sessions SET manager_session_id=? WHERE bridge_id=?`, realParent, childID); err != nil {
			t.Fatalf("seed live link: %v", err)
		}
		linked, err := s.LinkDiscoveredSessionParent(childID, "parent-uuid")
		if err != nil {
			t.Fatalf("link: %v", err)
		}
		if linked {
			t.Error("overwrote an existing manager_session_id; the live promotion is authoritative")
		}
		child, _ := s.GetSession(childID)
		if child.ManagerSessionID != realParent {
			t.Errorf("manager_session_id = %q, want %q untouched", child.ManagerSessionID, realParent)
		}
	})

	t.Run("leaves the row alone when the parent is not imported", func(t *testing.T) {
		s := newStore(t)
		childID := mkChild(t, s, "agent-a369d3bc")

		linked, err := s.LinkDiscoveredSessionParent(childID, "a-parent-nobody-imported")
		if err != nil {
			t.Fatalf("an unknown parent is not an error, it is a later pass: %v", err)
		}
		if linked {
			t.Error("invented a parent")
		}
		child, _ := s.GetSession(childID)
		if child.ManagerSessionID != "" {
			t.Errorf("manager_session_id = %q, want empty — empty means no parent, not a guess", child.ManagerSessionID)
		}
	})

	t.Run("refuses a bridge id in the harness slot", func(t *testing.T) {
		s := newStore(t)
		childID := mkChild(t, s, "agent-a369d3bc")

		if _, err := s.LinkDiscoveredSessionParent(childID, "br_1785954582792515821"); err == nil {
			t.Error("accepted a bridge id as a harness parent id; that contract violation must be loud")
		}
	})

	t.Run("will not link a session to itself", func(t *testing.T) {
		s := newStore(t)
		selfID := mkChild(t, s, "agent-self")

		linked, err := s.LinkDiscoveredSessionParent(selfID, "agent-self")
		if err != nil {
			t.Fatalf("link: %v", err)
		}
		if linked {
			t.Error("linked a session to itself")
		}
	})
}
