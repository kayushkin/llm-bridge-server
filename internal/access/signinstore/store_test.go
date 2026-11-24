package signinstore

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestALinkRedeemedTwiceAtOnceOpensOneSession: the link's row is locked while
// it is spent, so the second redeem waits and then finds it used.
func TestALinkRedeemedTwiceAtOnceOpensOneSession(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for round := range 5 {
		_, secret, err := store.MakeSignInLink(ctx, testPerson, SentByOperator, testAdministrator)
		if err != nil {
			t.Fatal(err)
		}
		var wait sync.WaitGroup
		results := make([]error, 4)
		for index := range results {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, _, results[index] = store.RedeemSignInLink(ctx, secret, SessionRequestFacts{}, func(string) error { return nil })
			}()
		}
		wait.Wait()
		opened := 0
		for _, err := range results {
			switch {
			case err == nil:
				opened++
			case !errors.Is(err, ErrLinkNotUsable):
				t.Fatalf("round %d: redeem failed with %v, want success or ErrLinkNotUsable", round, err)
			}
		}
		if opened != 1 {
			t.Fatalf("round %d: %d redeems of one link succeeded, want 1", round, opened)
		}
	}
	sessions, err := store.ListSessions(ctx, testPerson, 100)
	if err != nil || len(sessions) != 5 {
		t.Fatalf("sessions = %d (%v), want 5: one per link", len(sessions), err)
	}
}

func TestARefusedRedeemSpendsNothing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	_, secret, err := store.MakeSignInLink(ctx, testPerson, SentByOperator, testAdministrator)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("disabled meanwhile")
	if _, _, err := store.RedeemSignInLink(ctx, secret, SessionRequestFacts{}, func(string) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("refused redeem = %v, want the refusal unchanged", err)
	}
	session, _, err := store.RedeemSignInLink(ctx, secret, SessionRequestFacts{UserAgent: "browser"}, func(string) error { return nil })
	if err != nil || session.ID != "session_000001" || session.SignInLinkID != "sign_in_link_000001" {
		t.Fatalf("redeem after a refusal = %+v %v, want session_000001 from sign_in_link_000001", session, err)
	}
}

func TestADiscordAttemptSpentTwiceAtOnceIsSpentOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.insertDiscordAttempt(ctx, "state-hash", "browser-hash", discordPurposeSignIn, "", "", "https://example.test/"); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make([]error, 6)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[index] = store.spendDiscordAttempt(ctx, "state-hash")
		}()
	}
	wait.Wait()
	spent := 0
	for _, err := range results {
		switch {
		case err == nil:
			spent++
		case !errors.Is(err, ErrNotFound):
			t.Fatalf("spend failed with %v", err)
		}
	}
	if spent != 1 {
		t.Fatalf("%d spends succeeded, want 1", spent)
	}
}

func TestASessionIdOfAnotherFormNamesNoRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"session_1", "sign_in_link_000001", "000001", "session_00000x"} {
		if _, err := store.GetSession(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetSession(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := store.RevokeSession(ctx, id, testAdministrator, "test"); !errors.Is(err, ErrNotFound) {
			t.Errorf("RevokeSession(%q) = %v, want ErrNotFound", id, err)
		}
	}
}
