package parkedasks

import (
	"sync"
	"testing"
	"time"
)

func TestParkedAsks_DeliverHit(t *testing.T) {
	p := New()
	ch := p.Park("br1", "rid1")

	ok := p.Deliver("br1", "rid1", Decision{Behavior: "allow", Message: "ok"})
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
	p := New()
	if p.Deliver("br1", "missing", Decision{Behavior: "allow"}) {
		t.Fatalf("deliver returned true for an unknown request_id")
	}
	if p.Deliver("missing-bridge", "rid1", Decision{Behavior: "deny"}) {
		t.Fatalf("deliver returned true for an unknown bridge_id")
	}
}

func TestParkedAsks_DoubleResolve(t *testing.T) {
	p := New()
	ch := p.Park("br1", "rid1")
	if !p.Deliver("br1", "rid1", Decision{Behavior: "allow"}) {
		t.Fatalf("first deliver should succeed")
	}
	if p.Deliver("br1", "rid1", Decision{Behavior: "deny"}) {
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
	p := New()
	p.Park("br1", "rid1")
	p.Cancel("br1", "rid1")
	if p.Deliver("br1", "rid1", Decision{Behavior: "allow"}) {
		t.Fatalf("deliver after cancel should miss")
	}
}

func TestParkedAsks_DropSession(t *testing.T) {
	p := New()
	p.Park("br1", "rid1")
	p.Park("br1", "rid2")
	p.Park("br2", "rid3")
	p.DropSession("br1")
	if p.Deliver("br1", "rid1", Decision{Behavior: "allow"}) {
		t.Fatalf("br1/rid1 should have been dropped")
	}
	if p.Deliver("br1", "rid2", Decision{Behavior: "allow"}) {
		t.Fatalf("br1/rid2 should have been dropped")
	}
	if !p.Deliver("br2", "rid3", Decision{Behavior: "allow"}) {
		t.Fatalf("br2/rid3 should still be parked")
	}
}

// TestParkedAsks_ConcurrentDeliver verifies the buffered channel + map
// removal under contention — deliver and a slow reader must not race
// in a way that loses the decision. Run with -race to surface data races.
func TestParkedAsks_ConcurrentDeliver(t *testing.T) {
	p := New()
	const N = 64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rid := "rid" + string(rune('A'+i%26)) + string(rune('A'+i/26))
			ch := p.Park("br1", rid)
			go p.Deliver("br1", rid, Decision{Behavior: "allow", Message: rid})
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
	p := New()
	p.Park("br1", "rid1")
	p.Park("br1", "rid2")
	p.Deliver("br1", "rid1", Decision{Behavior: "allow"})

	p.mutex.Lock()
	if _, ok := p.asksBySession["br1"]; !ok {
		p.mutex.Unlock()
		t.Fatalf("bucket dropped early — rid2 still parked")
	}
	p.mutex.Unlock()

	p.Deliver("br1", "rid2", Decision{Behavior: "deny"})

	p.mutex.Lock()
	if _, ok := p.asksBySession["br1"]; ok {
		p.mutex.Unlock()
		t.Fatalf("empty bucket not removed from outer map")
	}
	p.mutex.Unlock()
}
