package harnessstore

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMintEnrollment(t *testing.T) {
	s := newTestStore(t)

	passphrase, enr, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	if len(passphrase) != passphraseLength {
		t.Errorf("passphrase len = %d, want %d", len(passphrase), passphraseLength)
	}
	for _, r := range passphrase {
		if !strings.ContainsRune(passphraseAlphabet, r) {
			t.Errorf("passphrase contains out-of-alphabet rune %q", r)
		}
	}
	if !strings.HasPrefix(enr.ID, "enr_") {
		t.Errorf("enrollment ID = %q, want enr_ prefix", enr.ID)
	}
	if enr.ExpiresAt.Before(enr.CreatedAt) {
		t.Errorf("ExpiresAt %v before CreatedAt %v", enr.ExpiresAt, enr.CreatedAt)
	}

	// Plaintext must not be recoverable from the table — only the hash is stored.
	var stored string
	if err := s.DB().QueryRow(`SELECT passphrase_hash FROM runner_enrollments WHERE id = ?`, enr.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if stored == passphrase {
		t.Error("plaintext passphrase stored instead of hash")
	}
	if stored != hashPassphrase(passphrase) {
		t.Error("stored hash does not match hashPassphrase(plaintext)")
	}
}

func TestMintEnrollmentDefaultTTL(t *testing.T) {
	s := newTestStore(t)
	// Non-positive TTL falls back to the 15-minute default.
	_, enr, err := s.MintEnrollment(0)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	ttl := enr.ExpiresAt.Sub(enr.CreatedAt)
	if ttl < 14*time.Minute || ttl > 16*time.Minute {
		t.Errorf("default TTL = %v, want ~15m", ttl)
	}
}

func TestConsumeEnrollmentHappyPath(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_enr", "enrolled")

	passphrase, enr, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}

	if err := s.ConsumeEnrollment(passphrase, "m_enr"); err != nil {
		t.Fatalf("ConsumeEnrollment: %v", err)
	}

	// used_at + consumed_machine_id are now set.
	var usedAt sql.NullTime
	var machineID sql.NullString
	if err := s.DB().QueryRow(`SELECT used_at, consumed_machine_id FROM runner_enrollments WHERE id = ?`, enr.ID).
		Scan(&usedAt, &machineID); err != nil {
		t.Fatalf("read consumed row: %v", err)
	}
	if !usedAt.Valid {
		t.Error("used_at not set after consume")
	}
	if machineID.String != "m_enr" {
		t.Errorf("consumed_machine_id = %q, want m_enr", machineID.String)
	}
}

func TestConsumeEnrollmentAlreadyUsed(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_enr", "enrolled")
	passphrase, _, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	if err := s.ConsumeEnrollment(passphrase, "m_enr"); err != nil {
		t.Fatalf("first ConsumeEnrollment: %v", err)
	}
	// Second consume of the same single-use passphrase is rejected.
	if err := s.ConsumeEnrollment(passphrase, "m_enr"); !errors.Is(err, ErrEnrollmentExpired) {
		t.Errorf("second consume err = %v, want ErrEnrollmentExpired", err)
	}
}

func TestConsumeEnrollmentExpired(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_enr", "enrolled")
	passphrase, enr, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment: %v", err)
	}
	// Backdate expiry so the row is past its TTL.
	if _, err := s.DB().Exec(`UPDATE runner_enrollments SET expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute), enr.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if err := s.ConsumeEnrollment(passphrase, "m_enr"); !errors.Is(err, ErrEnrollmentExpired) {
		t.Errorf("expired consume err = %v, want ErrEnrollmentExpired", err)
	}
}

func TestConsumeEnrollmentUnknown(t *testing.T) {
	s := newTestStore(t)
	// An unknown passphrase hashes to a row that does not exist → sql.ErrNoRows.
	if err := s.ConsumeEnrollment("nonexistent", "m_enr"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown consume err = %v, want sql.ErrNoRows", err)
	}
}

func TestPurgeExpiredEnrollments(t *testing.T) {
	s := newTestStore(t)
	mustCreateMachine(t, s, "m_enr", "enrolled")

	// One live, one expired-unused, one expired-but-consumed.
	_, live, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment live: %v", err)
	}
	_, expired, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment expired: %v", err)
	}
	usedPass, used, err := s.MintEnrollment(time.Hour)
	if err != nil {
		t.Fatalf("MintEnrollment used: %v", err)
	}
	if err := s.ConsumeEnrollment(usedPass, "m_enr"); err != nil {
		t.Fatalf("ConsumeEnrollment: %v", err)
	}

	past := time.Now().UTC().Add(-time.Minute)
	for _, id := range []string{expired.ID, used.ID} {
		if _, err := s.DB().Exec(`UPDATE runner_enrollments SET expires_at = ? WHERE id = ?`, past, id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	// Purge only removes expired AND unused rows — the consumed one is kept.
	n, err := s.PurgeExpiredEnrollments()
	if err != nil {
		t.Fatalf("PurgeExpiredEnrollments: %v", err)
	}
	if n != 1 {
		t.Errorf("purged = %d, want 1 (only expired-unused)", n)
	}

	remaining := map[string]bool{}
	rows, err := s.DB().Query(`SELECT id FROM runner_enrollments`)
	if err != nil {
		t.Fatalf("query remaining: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining[id] = true
	}
	if !remaining[live.ID] {
		t.Error("live enrollment was purged")
	}
	if !remaining[used.ID] {
		t.Error("consumed enrollment was purged (should be kept)")
	}
	if remaining[expired.ID] {
		t.Error("expired-unused enrollment survived purge")
	}
}

func TestGeneratePassphraseDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := generatePassphrase()
		if err != nil {
			t.Fatalf("generatePassphrase: %v", err)
		}
		if len(p) != passphraseLength {
			t.Fatalf("len = %d, want %d", len(p), passphraseLength)
		}
		seen[p] = true
	}
	// 50 draws from ~46 bits of entropy should essentially never collide.
	if len(seen) != 50 {
		t.Errorf("got %d distinct passphrases out of 50", len(seen))
	}
}
