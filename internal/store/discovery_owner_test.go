package store

import (
	"testing"
	"time"
)

func TestDiscoveredSessionOwnerPreservedAndUnchangedSilent(t *testing.T) {
	for _, initialOwner := range []string{"", "original-owner"} {
		t.Run(initialOwner, func(t *testing.T) {
			s := testStore(t)
			notifier := &countingNotifier{changed: map[string]int{}}
			s.SetNotifier(notifier)
			now := time.Now().UTC()
			upsert := func(owner string) (string, bool, error) {
				return s.UpsertDiscoveredSessionOwnedBy("local-chat", "", "Chat", "codex", "local", "chat", "", now, now, owner)
			}
			id, inserted, err := upsert(initialOwner)
			if err != nil || !inserted {
				t.Fatalf("insert: %v %v", inserted, err)
			}
			again, inserted, err := upsert("configured-owner")
			if err != nil || inserted || again != id {
				t.Fatalf("rediscover: %s %v %v", again, inserted, err)
			}
			sess, err := s.GetSession(id)
			if err != nil {
				t.Fatal(err)
			}
			want := initialOwner
			wantNotifications := 1
			if want == "" {
				want = "configured-owner"
				wantNotifications++
			}
			if sess.PrincipalID != want {
				t.Fatalf("owner = %q, want %q", sess.PrincipalID, want)
			}
			if _, _, err := upsert("configured-owner"); err != nil {
				t.Fatal(err)
			}
			if notifier.changed[id] != wantNotifications {
				t.Fatalf("notifications = %d, want %d", notifier.changed[id], wantNotifications)
			}
			own, err := s.ListSessionsOwnedByPrincipalPaged(want, "", "", 10, 0)
			if err != nil || len(own) != 1 {
				t.Fatalf("owner cannot see imported session: %v %v", own, err)
			}
		})
	}
}
