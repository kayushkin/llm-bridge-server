package server

import (
	"sync"
	"testing"
	"time"
)

func TestParkedAsks_DeliverHit(t *testing.T) {
	p := newParkedAsks()
	ch := p.park("br1", "rid1")

	ok := p.deliver("br1", "rid1", permissionDecision{Behavior: "allow", Message: "ok"})
	if !ok {
		t.Fatalf("deliver returned false on a parked entry")
	}

	select {
	case d := <-ch:
		if d.Behavior != "allow" || d.Message != "ok" {
			t.Fatalf("unexpected decision: %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatalf("decision never landed on channel")
	}
}

func TestParkedAsks_DeliverMiss(t *testing.T) {
	p := newParkedAsks()
	if p.deliver("br1", "missing", permissionDecision{Behavior: "allow"}) {
		t.Fatalf("deliver returned true for an unknown request_id")
	}
	if p.deliver("missing-bridge", "rid1", permissionDecision{Behavior: "deny"}) {
		t.Fatalf("deliver returned true for an unknown bridge_id")
	}
}

func TestParkedAsks_DoubleResolve(t *testing.T) {
	p := newParkedAsks()
	ch := p.park("br1", "rid1")
	if !p.deliver("br1", "rid1", permissionDecision{Behavior: "allow"}) {
		t.Fatalf("first deliver should succeed")
	}
	if p.deliver("br1", "rid1", permissionDecision{Behavior: "deny"}) {
		t.Fatalf("second deliver should miss — entry is consumed")
	}
	select {
	case d := <-ch:
		if d.Behavior != "allow" {
			t.Fatalf("first decision should still be allow, got %q", d.Behavior)
		}
	default:
		t.Fatalf("first decision missing from channel")
	}
}

func TestParkedAsks_Cancel(t *testing.T) {
	p := newParkedAsks()
	p.park("br1", "rid1")
	p.cancel("br1", "rid1")
	if p.deliver("br1", "rid1", permissionDecision{Behavior: "allow"}) {
		t.Fatalf("deliver after cancel should miss")
	}
}

func TestParkedAsks_DropSession(t *testing.T) {
	p := newParkedAsks()
	p.park("br1", "rid1")
	p.park("br1", "rid2")
	p.park("br2", "rid3")
	p.dropSession("br1")
	if p.deliver("br1", "rid1", permissionDecision{Behavior: "allow"}) {
		t.Fatalf("br1/rid1 should have been dropped")
	}
	if p.deliver("br1", "rid2", permissionDecision{Behavior: "allow"}) {
		t.Fatalf("br1/rid2 should have been dropped")
	}
	if !p.deliver("br2", "rid3", permissionDecision{Behavior: "allow"}) {
		t.Fatalf("br2/rid3 should still be parked")
	}
}

// TestParkedAsks_ConcurrentDeliver verifies the buffered channel + map
// removal under contention — deliver and a slow reader must not race
// in a way that loses the decision. Run with -race to surface data races.
func TestParkedAsks_ConcurrentDeliver(t *testing.T) {
	p := newParkedAsks()
	const N = 64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rid := "rid" + string(rune('A'+i%26)) + string(rune('A'+i/26))
			ch := p.park("br1", rid)
			go p.deliver("br1", rid, permissionDecision{Behavior: "allow", Message: rid})
			select {
			case d := <-ch:
				if d.Message != rid {
					t.Errorf("rid %s: got message %q", rid, d.Message)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("rid %s: deliver lost", rid)
			}
		}()
	}
	wg.Wait()
}

// TestParkedAsks_SessionEmptyOnLastDeliver checks that a session bucket is
// removed from the outer map when its last entry resolves — prevents
// per-session memory leaks across long-running servers.
func TestParkedAsks_SessionEmptyOnLastDeliver(t *testing.T) {
	p := newParkedAsks()
	p.park("br1", "rid1")
	p.park("br1", "rid2")
	p.deliver("br1", "rid1", permissionDecision{Behavior: "allow"})

	p.mu.Lock()
	if _, ok := p.m["br1"]; !ok {
		p.mu.Unlock()
		t.Fatalf("bucket dropped early — rid2 still parked")
	}
	p.mu.Unlock()

	p.deliver("br1", "rid2", permissionDecision{Behavior: "deny"})

	p.mu.Lock()
	if _, ok := p.m["br1"]; ok {
		p.mu.Unlock()
		t.Fatalf("empty bucket not removed from outer map")
	}
	p.mu.Unlock()
}
