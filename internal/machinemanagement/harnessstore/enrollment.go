package harnessstore

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ErrEnrollmentExpired is returned when an enrollment passphrase exists
// but has either passed its TTL or already been used.
var ErrEnrollmentExpired = errors.New("enrollment passphrase expired or already used")

// passphraseAlphabet is an unambiguous human-writable alphabet — no 0/O,
// 1/I/l, etc. — so passphrases survive being read aloud or copied by hand.
const passphraseAlphabet = "abcdefghjkmnpqrstuvwxyzACDEFHJKLMNPQRSTUVWXYZ23456789"

// passphraseLength is 8 chars over the 52-char alphabet → ~46 bits.
// Combined with single-use semantics + 15-min default TTL, this is far
// past brute-force feasibility on any practical attack model.
const passphraseLength = 8

// MintEnrollment creates a new single-use enrollment passphrase. The
// plaintext is returned to the caller (this is the only chance to capture
// it); only its hash is persisted.
func (s *Store) MintEnrollment(ttl time.Duration) (passphrase string, enr *msg.RunnerEnrollment, err error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	plaintext, err := generatePassphrase()
	if err != nil {
		return "", nil, err
	}
	hash := hashPassphrase(plaintext)
	id := "enr_" + hash[:12]

	now := time.Now().UTC()
	expires := now.Add(ttl)

	_, err = s.db.Exec(`
		INSERT INTO runner_enrollments (id, passphrase_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?)`,
		id, hash, expires, now,
	)
	if err != nil {
		return "", nil, err
	}
	return plaintext, &msg.RunnerEnrollment{
		ID:        id,
		ExpiresAt: expires,
		CreatedAt: now,
	}, nil
}

// ConsumeEnrollment redeems an enrollment passphrase, marking it used and
// binding it to the supplied machine. Caller must invoke this inside the
// same logical operation as creating the machine — caller is responsible
// for atomicity.
//
// Returns ErrEnrollmentExpired when the row is missing, expired, or
// already used. Returns sql.ErrNoRows when the passphrase is unknown.
func (s *Store) ConsumeEnrollment(passphrase, machineID string) error {
	hash := hashPassphrase(passphrase)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		id          string
		expiresAt   time.Time
		usedAt      sql.NullTime
		consumedMID sql.NullString
	)
	err = tx.QueryRow(`
		SELECT id, expires_at, used_at, consumed_machine_id
		FROM runner_enrollments WHERE passphrase_hash = ?`, hash).
		Scan(&id, &expiresAt, &usedAt, &consumedMID)
	if err != nil {
		return err
	}
	if usedAt.Valid {
		return fmt.Errorf("%w: already used at %s", ErrEnrollmentExpired, usedAt.Time.Format(time.RFC3339))
	}
	if time.Now().UTC().After(expiresAt) {
		return fmt.Errorf("%w: expired at %s", ErrEnrollmentExpired, expiresAt.Format(time.RFC3339))
	}

	if _, err := tx.Exec(`
		UPDATE runner_enrollments SET used_at = ?, consumed_machine_id = ? WHERE id = ?`,
		time.Now().UTC(), machineID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// PurgeExpiredEnrollments deletes enrollments past their TTL that were
// never consumed. Keeps the table from growing unboundedly when an admin
// mints many that never get redeemed.
func (s *Store) PurgeExpiredEnrollments() (int, error) {
	res, err := s.db.Exec(`DELETE FROM runner_enrollments WHERE used_at IS NULL AND expires_at < ?`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// generatePassphrase returns a passphraseLength-char string from the
// human-writable alphabet, drawn from crypto/rand.
func generatePassphrase() (string, error) {
	out := make([]byte, passphraseLength)
	max := byte(len(passphraseAlphabet))
	// Reject-sample to avoid modulo bias on non-power-of-two alphabets.
	cap := byte(256 - (256 % int(max)))
	buf := make([]byte, 1)
	for i := 0; i < passphraseLength; {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if buf[0] >= cap {
			continue
		}
		out[i] = passphraseAlphabet[buf[0]%max]
		i++
	}
	return string(out), nil
}

// hashPassphrase returns the canonical hex sha256 of a passphrase. Used
// both at mint (to store) and at consume (to look up). sha256 is fine
// here — the passphrase itself has 46 bits of entropy and is single-use,
// so we don't need a slow KDF; we just need a deterministic lookup key
// that doesn't expose the plaintext at rest.
func hashPassphrase(p string) string {
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:])
}

// HashRunnerToken returns the canonical hex sha256 of a runner bearer
// token. Reused by the server-side WS auth path.
func HashRunnerToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// GenerateRunnerToken returns a fresh 256-bit runner token, hex-encoded.
// Issued on enrollment, stored locally on the runner machine.
func GenerateRunnerToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
