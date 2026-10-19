package store

import (
	"errors"
	"testing"
	"time"
)

func insertTestLoginSession(t *testing.T, s *Store, tokenHash, principalID, identityBindingID string, createdAt time.Time) LoginSession {
	t.Helper()
	method := LoginSessionMethodDemo
	if identityBindingID != "" {
		method = LoginSessionMethodEntra
	}
	session := LoginSession{
		TokenHash: tokenHash, CSRFToken: "csrf-" + tokenHash, PrincipalID: principalID, Method: method,
		IdentityBindingID: identityBindingID, CreatedAt: createdAt, ExpiresAt: createdAt.Add(12 * time.Hour), LastSeenAt: createdAt,
		UserAgent: "test-agent", RemoteAddress: "192.0.2.1:1234",
	}
	if err := s.InsertLoginSession(&session); err != nil {
		t.Fatalf("insert login session: %v", err)
	}
	return session
}

func TestLoginSessionIsFoundByItsTokenHashAndRevokedOnce(t *testing.T) {
	s := testStore(t)
	createdAt := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	inserted := insertTestLoginSession(t, s, "hash-one", "principal_000001", "", createdAt)
	if inserted.ID != "login_session_000001" {
		t.Fatalf("id = %q, want login_session_000001", inserted.ID)
	}

	found, err := s.GetLoginSessionByTokenHash("hash-one")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if found.PrincipalID != "principal_000001" || found.Method != LoginSessionMethodDemo || found.CSRFToken != "csrf-hash-one" ||
		!found.ExpiresAt.Equal(createdAt.Add(12*time.Hour)) || found.RevokedAt != nil || found.UserAgent != "test-agent" {
		t.Fatalf("found %+v, want the inserted row", found)
	}
	if _, err := s.GetLoginSessionByTokenHash("hash-unknown"); !errors.Is(err, ErrLoginSessionNotFound) {
		t.Fatalf("unknown hash: %v, want ErrLoginSessionNotFound", err)
	}

	seenAt := createdAt.Add(time.Hour)
	if err := s.TouchLoginSession(inserted.ID, seenAt); err != nil {
		t.Fatal(err)
	}
	revokedAt := createdAt.Add(2 * time.Hour)
	revoked, err := s.RevokeLoginSession(inserted.ID, "principal_000003", "lost laptop", revokedAt)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) || revoked.RevokedBy != "principal_000003" ||
		revoked.RevokeReason != "lost laptop" || !revoked.LastSeenAt.Equal(seenAt) {
		t.Fatalf("revoked row %+v", revoked)
	}
	again, err := s.RevokeLoginSession(inserted.ID, "principal_000001", "second", revokedAt.Add(time.Minute))
	if !errors.Is(err, ErrLoginSessionAlreadyRevoked) || again.RevokeReason != "lost laptop" {
		t.Fatalf("second revoke = %+v, %v; want ErrLoginSessionAlreadyRevoked with the first reason kept", again, err)
	}
	if _, err := s.RevokeLoginSession("login_session_000099", "x", "y", revokedAt); !errors.Is(err, ErrLoginSessionNotFound) {
		t.Fatalf("revoke of a missing session: %v", err)
	}
}

func TestTokenHashIsUnique(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	insertTestLoginSession(t, s, "same-hash", "principal_000001", "", now)
	duplicate := LoginSession{TokenHash: "same-hash", CSRFToken: "c", PrincipalID: "principal_000002", Method: LoginSessionMethodDemo,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
	if err := s.InsertLoginSession(&duplicate); err == nil {
		t.Fatal("a second session with the same token hash was inserted")
	}
}

func TestListLoginSessionsOfPrincipalIsNewestFirstAndOnlyTheirs(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	first := insertTestLoginSession(t, s, "a", "principal_000001", "", now)
	insertTestLoginSession(t, s, "b", "principal_000002", "", now)
	third := insertTestLoginSession(t, s, "c", "principal_000001", "", now)
	listed, err := s.ListLoginSessionsOfPrincipal("principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != third.ID || listed[1].ID != first.ID {
		t.Fatalf("listed %+v, want %s then %s", listed, third.ID, first.ID)
	}
}

func TestRevokingAnIdentityBindingsSessionsLeavesOthersAlone(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	live := insertTestLoginSession(t, s, "live", "principal_000001", "identity_binding_000001", now)
	expired := insertTestLoginSession(t, s, "expired", "principal_000001", "identity_binding_000001", now.Add(-13*time.Hour))
	alreadyRevoked := insertTestLoginSession(t, s, "revoked", "principal_000001", "identity_binding_000001", now)
	if _, err := s.RevokeLoginSession(alreadyRevoked.ID, "principal_000001", "logout", now); err != nil {
		t.Fatal(err)
	}
	otherBinding := insertTestLoginSession(t, s, "other", "principal_000001", "identity_binding_000002", now)
	demo := insertTestLoginSession(t, s, "demo", "principal_000001", "", now)

	revokedIDs, err := s.RevokeLiveLoginSessionsOfIdentityBinding("identity_binding_000001", "principal_000003", "identity unlinked: left", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(revokedIDs) != 1 || revokedIDs[0] != live.ID {
		t.Fatalf("revoked %v, want only %s", revokedIDs, live.ID)
	}
	for _, check := range []struct {
		id          string
		wantRevoked bool
		wantReason  string
	}{
		{live.ID, true, "identity unlinked: left"},
		{expired.ID, false, ""},
		{alreadyRevoked.ID, true, "logout"},
		{otherBinding.ID, false, ""},
		{demo.ID, false, ""},
	} {
		row, err := s.GetLoginSession(check.id)
		if err != nil {
			t.Fatal(err)
		}
		if (row.RevokedAt != nil) != check.wantRevoked || row.RevokeReason != check.wantReason {
			t.Errorf("%s: revoked_at %v reason %q, want revoked %v reason %q", check.id, row.RevokedAt, row.RevokeReason, check.wantRevoked, check.wantReason)
		}
	}
}

func TestLoginAttemptIsConsumedOnceAndFinishedOnce(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	attempt := LoginAttempt{Provider: "entra", StateHash: "state-hash", NonceHash: "nonce-hash", CodeVerifier: "verifier",
		BrowserBindingHash: "binding-hash", ReturnTo: "/chat", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	if err := s.InsertLoginAttempt(&attempt); err != nil {
		t.Fatal(err)
	}
	if attempt.ID != "login_attempt_000001" {
		t.Fatalf("id = %q", attempt.ID)
	}
	consumed, err := s.ConsumeLoginAttemptByStateHash("state-hash", now)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if consumed.CodeVerifier != "verifier" || consumed.ConsumedAt == nil || consumed.ReturnTo != "/chat" {
		t.Fatalf("consumed %+v, want the verifier and a consumed stamp", consumed)
	}
	replayed, err := s.ConsumeLoginAttemptByStateHash("state-hash", now.Add(time.Second))
	if !errors.Is(err, ErrLoginAttemptAlreadyConsumed) || replayed.CodeVerifier != "" {
		t.Fatalf("replay = %+v, %v; want ErrLoginAttemptAlreadyConsumed and the verifier blanked", replayed, err)
	}
	if _, err := s.ConsumeLoginAttemptByStateHash("other", now); !errors.Is(err, ErrLoginAttemptNotFound) {
		t.Fatalf("unknown state: %v", err)
	}

	finished, err := s.FinishLoginAttempt(attempt.ID, LoginAttemptEnd{Outcome: "identity_not_provisioned", TenantID: "t", Subject: "o", PreferredUsername: "a@b"})
	if err != nil || finished.Outcome != "identity_not_provisioned" || finished.Subject != "o" {
		t.Fatalf("finish = %+v, %v", finished, err)
	}
	if _, err := s.FinishLoginAttempt(attempt.ID, LoginAttemptEnd{Outcome: LoginAttemptOutcomeSignedIn}); err == nil {
		t.Fatal("a finished attempt was finished again")
	}
	listed, err := s.ListLoginAttempts("identity_not_provisioned", 10)
	if err != nil || len(listed) != 1 || listed[0].ID != attempt.ID {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if listed, _ := s.ListLoginAttempts(LoginAttemptOutcomeSignedIn, 10); len(listed) != 0 {
		t.Fatalf("filter by outcome returned %+v", listed)
	}
}
