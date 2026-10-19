package localmail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/mailstack/backend"
)

// ============================ outbox ============================

// Event kinds.
const (
	// EventRevisionAvailable: a message revision was stored and can be read.
	// It says nothing about tickets: mailstack accepted the message, and what
	// any consumer does with it is the consumer's to record.
	EventRevisionAvailable = "message.revision_available"
	// EventPurged: a message and all its revisions are gone, for good.
	EventPurged = "message.purged"
)

// Event origins: which way in the change came.
const (
	EventOriginImport  = "import"
	EventOriginIngress = "ingress"
	EventOriginPurge   = "purge"
)

// MaxEventPage bounds one read of the outbox.
const MaxEventPage = 1000

// Event is one row of the outbox.
type Event struct {
	Seq               int64     `json:"seq"`
	Kind              string    `json:"kind"`
	Origin            string    `json:"origin"`
	AccountID         string    `json:"account_id"`
	ID                string    `json:"id"`
	Locator           string    `json:"locator"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	Revision          int64     `json:"revision,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

func queueEvent(ctx context.Context, tx *sql.Tx, e Event, now int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_events (kind, origin, account_id, id, provider_message_id, revision, created_at)
		VALUES (?,?,?,?,?,?,?)`, e.Kind, e.Origin, e.AccountID, e.ID, e.ProviderMessageID, e.Revision, now); err != nil {
		return fmt.Errorf("localmail: queue %s event for %s: %w", e.Kind, e.ID, err)
	}
	return nil
}

// Events reads the outbox after a cursor, oldest first. accountID narrows to
// one account when set. A consumer stores the last seq it handled and asks
// again from there; reading never removes anything.
func (s *Store) Events(ctx context.Context, after int64, accountID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > MaxEventPage {
		limit = MaxEventPage
	}
	query := `SELECT seq, kind, origin, account_id, id, provider_message_id, revision, created_at
		FROM message_events WHERE seq > ?`
	args := []any{after}
	if accountID != "" {
		query += ` AND account_id = ?`
		args = append(args, accountID)
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("localmail: read events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Origin, &e.AccountID, &e.ID, &e.ProviderMessageID, &e.Revision, &createdAt); err != nil {
			return nil, fmt.Errorf("localmail: scan event: %w", err)
		}
		e.Locator = e.AccountID + ":" + e.ID
		e.CreatedAt = time.UnixMilli(createdAt).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ============================ purges ============================

// PurgedError is a message that was purged and may not come back.
type PurgedError struct {
	AccountID, ProviderMessageID, Reason string
	PurgedAt                             time.Time
}

func (e *PurgedError) Error() string {
	return fmt.Sprintf("localmail: message %s in %s was purged at %s (%s) and is not taken again",
		e.ProviderMessageID, e.AccountID, e.PurgedAt.Format(time.RFC3339), e.Reason)
}

func refusePurged(ctx context.Context, tx *sql.Tx, accountID, providerMessageID string) error {
	var reason string
	var purgedAt int64
	err := tx.QueryRowContext(ctx, `SELECT reason, purged_at FROM message_purges WHERE account_id = ? AND provider_message_id = ?`,
		accountID, providerMessageID).Scan(&reason, &purgedAt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("localmail: check purge of %s: %w", providerMessageID, err)
	}
	return &PurgedError{AccountID: accountID, ProviderMessageID: providerMessageID, Reason: reason, PurgedAt: time.UnixMilli(purgedAt).UTC()}
}

// Purge destroys a message and every revision of it, remembers that it was
// purged so its source id is refused from now on, and announces it. The
// caller purges the attachment files first; see AttachmentFileIDs.
func (s *Store) Purge(ctx context.Context, accountID, id, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("localmail: a purge needs a reason")
	}
	now := time.Now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("localmail: begin purge: %w", err)
	}
	defer tx.Rollback()
	var providerMessageID string
	err = tx.QueryRowContext(ctx, `SELECT provider_message_id FROM local_messages WHERE account_id = ? AND id = ?`, accountID, id).Scan(&providerMessageID)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("localmail: read message to purge: %w", err)
	}
	if providerMessageID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_purges (account_id, provider_message_id, message_id, reason, purged_at)
			VALUES (?,?,?,?,?)`, accountID, providerMessageID, id, reason, now); err != nil {
			return fmt.Errorf("localmail: record purge of %s: %w", id, err)
		}
	}
	for _, table := range []string{"local_messages", "local_message_bodies", "local_message_revisions", "local_message_attachments"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE account_id = ? AND id = ?`, accountID, id); err != nil {
			return fmt.Errorf("localmail: purge from %s: %w", table, err)
		}
	}
	if err := queueEvent(ctx, tx, Event{Kind: EventPurged, Origin: EventOriginPurge, AccountID: accountID, ID: id,
		ProviderMessageID: providerMessageID}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("localmail: commit purge: %w", err)
	}
	return nil
}

// ============================ credentials ============================

// ErrCredentialNotFound is an ingress credential that does not exist, or a
// token that names none.
var ErrCredentialNotFound = errors.New("localmail: no such ingress credential")

// IngressCredential is one sender's right to deliver into named accounts. The
// token is shown once, at creation; only its hash is kept.
type IngressCredential struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	Note       string     `json:"note,omitempty"`
	AccountIDs []string   `json:"account_ids"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// MayDeliverTo reports whether the credential is bound to accountID.
func (c IngressCredential) MayDeliverTo(accountID string) bool {
	for _, id := range c.AccountIDs {
		if id == accountID {
			return true
		}
	}
	return false
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateIngressCredential mints a credential for source, bound to accountIDs,
// and returns it with its token. The caller checks the accounts exist.
func (s *Store) CreateIngressCredential(ctx context.Context, source, note string, accountIDs []string) (IngressCredential, string, error) {
	if strings.TrimSpace(source) == "" {
		return IngressCredential{}, "", fmt.Errorf("localmail: a credential needs a source")
	}
	if len(accountIDs) == 0 {
		return IngressCredential{}, "", fmt.Errorf("localmail: a credential needs at least one account")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return IngressCredential{}, "", fmt.Errorf("localmail: mint token: %w", err)
	}
	token := "msi_" + hex.EncodeToString(raw)
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return IngressCredential{}, "", fmt.Errorf("localmail: mint credential id: %w", err)
	}
	credential := IngressCredential{ID: "ingress_" + hex.EncodeToString(idBytes), Source: source, Note: note,
		AccountIDs: accountIDs, CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngressCredential{}, "", fmt.Errorf("localmail: begin credential: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ingress_credentials (credential_id, token_sha256, source, note, created_at) VALUES (?,?,?,?,?)`,
		credential.ID, tokenHash(token), source, note, credential.CreatedAt.UnixMilli()); err != nil {
		return IngressCredential{}, "", fmt.Errorf("localmail: store credential: %w", err)
	}
	for _, accountID := range accountIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingress_credential_accounts (credential_id, account_id) VALUES (?,?)`,
			credential.ID, accountID); err != nil {
			return IngressCredential{}, "", fmt.Errorf("localmail: bind credential to %s: %w", accountID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return IngressCredential{}, "", fmt.Errorf("localmail: commit credential: %w", err)
	}
	return credential, token, nil
}

func (s *Store) credentialWhere(ctx context.Context, where string, arg any) ([]IngressCredential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT credential_id, source, note, created_at, revoked_at FROM ingress_credentials WHERE `+where+` ORDER BY created_at`, arg)
	if err != nil {
		return nil, fmt.Errorf("localmail: read credentials: %w", err)
	}
	var out []IngressCredential
	for rows.Next() {
		var c IngressCredential
		var createdAt, revokedAt int64
		if err := rows.Scan(&c.ID, &c.Source, &c.Note, &createdAt, &revokedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("localmail: scan credential: %w", err)
		}
		c.CreatedAt = time.UnixMilli(createdAt).UTC()
		if revokedAt != 0 {
			revoked := time.UnixMilli(revokedAt).UTC()
			c.RevokedAt = &revoked
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localmail: read credentials: %w", err)
	}
	for i := range out {
		accountRows, err := s.db.QueryContext(ctx, `SELECT account_id FROM ingress_credential_accounts WHERE credential_id = ? ORDER BY account_id`, out[i].ID)
		if err != nil {
			return nil, fmt.Errorf("localmail: read credential accounts: %w", err)
		}
		out[i].AccountIDs = []string{}
		for accountRows.Next() {
			var accountID string
			if err := accountRows.Scan(&accountID); err != nil {
				accountRows.Close()
				return nil, fmt.Errorf("localmail: scan credential account: %w", err)
			}
			out[i].AccountIDs = append(out[i].AccountIDs, accountID)
		}
		accountRows.Close()
	}
	return out, nil
}

// IngressCredentials lists every credential, revoked ones included.
func (s *Store) IngressCredentials(ctx context.Context) ([]IngressCredential, error) {
	out, err := s.credentialWhere(ctx, `1 = ?`, 1)
	if out == nil && err == nil {
		out = []IngressCredential{}
	}
	return out, err
}

// IngressCredentialByToken finds the live credential a token names.
func (s *Store) IngressCredentialByToken(ctx context.Context, token string) (IngressCredential, error) {
	found, err := s.credentialWhere(ctx, `token_sha256 = ?`, tokenHash(token))
	if err != nil {
		return IngressCredential{}, err
	}
	if len(found) != 1 || found[0].RevokedAt != nil {
		return IngressCredential{}, ErrCredentialNotFound
	}
	return found[0], nil
}

// RevokeIngressCredential stops a credential from delivering. It is kept, so
// its receipts still name it.
func (s *Store) RevokeIngressCredential(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE ingress_credentials SET revoked_at = ? WHERE credential_id = ? AND revoked_at = 0`,
		time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("localmail: revoke credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingress_credentials WHERE credential_id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("localmail: check credential: %w", err)
		}
		if exists == 0 {
			return ErrCredentialNotFound
		}
	}
	return nil
}

// ============================ deliveries ============================

// Delivery statuses. accepted and replay mean the message is stored; the
// others mean it is not, and sending it again will not change that. Only
// failed is worth retrying.
const (
	DeliveryAccepted = "accepted"
	DeliveryReplay   = "replay"
	DeliveryConflict = "conflict"
	DeliveryPurged   = "purged"
	DeliveryRejected = "rejected"
	DeliveryFailed   = "failed"
)

// MaxDeliveryBatch bounds one push.
const MaxDeliveryBatch = 100

// Delivery is one message a sender pushes, addressed to one account.
type Delivery struct {
	// DeliveryID is the sender's id for this delivery attempt's content. The
	// same id sent again is answered from its receipt.
	DeliveryID string          `json:"delivery_id"`
	AccountID  string          `json:"account_id"`
	Message    ImportedMessage `json:"message"`
}

// DeliveryResult is the answer to one delivery, and what its receipt holds.
type DeliveryResult struct {
	DeliveryID       string `json:"delivery_id"`
	AccountID        string `json:"account_id"`
	Status           string `json:"status"`
	ReceiptID        int64  `json:"receipt_id,omitempty"`
	ID               string `json:"id,omitempty"`
	Locator          string `json:"locator,omitempty"`
	Revision         int64  `json:"revision,omitempty"`
	PreviousRevision int64  `json:"previous_revision,omitempty"`
	// Redelivered is set when this delivery id was answered before; the
	// answer is the one it got then.
	Redelivered bool   `json:"redelivered,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Retry       bool   `json:"retry"`

	changed       *storedMessage
	unusedFileIDs []string
}

// ChangedMeta is the index's view of the message when the delivery created or
// updated one, for the caller to index; nil otherwise.
func (r DeliveryResult) ChangedMeta() *backend.MessageMeta {
	if r.changed == nil {
		return nil
	}
	return r.changed.changed
}

// UnusedFileIDs are uploads the delivery did not keep.
func (r DeliveryResult) UnusedFileIDs() []string { return r.unusedFileIDs }

// ValidateDelivery checks what an import checks, and what a push needs on top:
// a delivery id, an account, and a source revision, which is the delivery's
// identity together with the source's message id.
func ValidateDelivery(d Delivery) error {
	var problems []string
	if strings.TrimSpace(d.DeliveryID) == "" {
		problems = append(problems, "delivery_id is required")
	}
	if strings.TrimSpace(d.AccountID) == "" {
		problems = append(problems, "account_id is required")
	}
	if strings.TrimSpace(d.Message.SourceRevision) == "" {
		problems = append(problems, "message.source_revision is required: with provider_message_id it is what makes a redelivery recognisable")
	}
	var invalid *ImportValidationError
	if err := ValidateImport([]ImportedMessage{d.Message}); errors.As(err, &invalid) {
		for _, item := range invalid.Items {
			problems = append(problems, "message: "+item.Error)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Deliver stores one delivery and its receipt in one transaction, and with
// the message its outbox event, so the sender's acknowledgement is never ahead
// of what is durable. The caller has validated it, checked the credential is
// bound to the account, and uploaded its attachments.
//
// Same source revision and same content: a replay, nothing changes. Same
// source revision and different content: a conflict, nothing changes, and the
// receipt keeps the evidence. A new source revision: a new revision here.
func (s *Store) Deliver(ctx context.Context, credential IngressCredential, d Delivery) (DeliveryResult, error) {
	m := d.Message
	result := DeliveryResult{DeliveryID: d.DeliveryID, AccountID: d.AccountID}
	hash, err := contentHash(m)
	if err != nil {
		return DeliveryResult{}, err
	}
	now := time.Now().UTC().UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("localmail: begin delivery: %w", err)
	}
	defer tx.Rollback()

	// A delivery id answered before gets the same answer.
	var earlier DeliveryResult
	var earlierHash string
	err = tx.QueryRowContext(ctx, `SELECT receipt_id, status, message_id, revision, detail, content_sha256
		FROM delivery_receipts WHERE source = ? AND delivery_id = ?`, credential.Source, d.DeliveryID).
		Scan(&earlier.ReceiptID, &earlier.Status, &earlier.ID, &earlier.Revision, &earlier.Detail, &earlierHash)
	switch {
	case err == nil:
		earlier.DeliveryID, earlier.AccountID, earlier.Redelivered = d.DeliveryID, d.AccountID, true
		if earlier.ID != "" {
			earlier.Locator = d.AccountID + ":" + earlier.ID
		}
		if earlierHash != hash {
			earlier.Status = DeliveryConflict
			earlier.Detail = fmt.Sprintf("delivery id %s was answered before (receipt %d) for different content", d.DeliveryID, earlier.ReceiptID)
		}
		for _, a := range m.Attachments {
			if a.FileID != "" {
				earlier.unusedFileIDs = append(earlier.unusedFileIDs, a.FileID)
			}
		}
		return earlier, nil
	case err != sql.ErrNoRows:
		return DeliveryResult{}, fmt.Errorf("localmail: look up delivery %s: %w", d.DeliveryID, err)
	}

	id := ImportedID(d.AccountID, m.ProviderMessageID)
	recordedStatus := ""
	if err := refusePurged(ctx, tx, d.AccountID, m.ProviderMessageID); err != nil {
		var purged *PurgedError
		if !errors.As(err, &purged) {
			return DeliveryResult{}, err
		}
		recordedStatus, result.Detail = DeliveryPurged, purged.Error()
	}
	if recordedStatus == "" {
		recordedStatus, result.Revision, result.Detail, err = sameSourceRevision(ctx, tx, d.AccountID, id, m.SourceRevision, hash)
		if err != nil {
			return DeliveryResult{}, err
		}
	}
	if recordedStatus == "" {
		stored, err := storeImportedMessage(ctx, tx, d.AccountID, m, EventOriginIngress, now)
		if err != nil {
			return DeliveryResult{}, err
		}
		recordedStatus = DeliveryAccepted
		result.Revision, result.PreviousRevision = stored.outcome.Revision, stored.outcome.PreviousRevision
		result.unusedFileIDs = stored.unusedFileIDs
		result.changed = &stored
	}
	if recordedStatus != DeliveryAccepted {
		for _, a := range m.Attachments {
			if a.FileID != "" {
				result.unusedFileIDs = append(result.unusedFileIDs, a.FileID)
			}
		}
	}
	result.Status = recordedStatus
	if recordedStatus == DeliveryAccepted || recordedStatus == DeliveryReplay {
		result.ID, result.Locator = id, d.AccountID+":"+id
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO delivery_receipts (credential_id, source, delivery_id, account_id,
		provider_message_id, source_revision, content_sha256, status, message_id, revision, detail, received_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, credential.ID, credential.Source, d.DeliveryID, d.AccountID,
		m.ProviderMessageID, m.SourceRevision, hash, result.Status, result.ID, result.Revision, result.Detail, now)
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("localmail: record receipt for %s: %w", d.DeliveryID, err)
	}
	if result.ReceiptID, err = res.LastInsertId(); err != nil {
		return DeliveryResult{}, fmt.Errorf("localmail: receipt id for %s: %w", d.DeliveryID, err)
	}
	if err := tx.Commit(); err != nil {
		return DeliveryResult{}, fmt.Errorf("localmail: commit delivery %s: %w", d.DeliveryID, err)
	}
	return result, nil
}

// sameSourceRevision checks whether this source revision of the message is
// already here, current or replaced. It answers replay for the same content,
// conflict for different content, and "" when the revision is new.
func sameSourceRevision(ctx context.Context, tx *sql.Tx, accountID, id, sourceRevision, hash string) (string, int64, string, error) {
	var revision int64
	var storedHash string
	err := tx.QueryRowContext(ctx, `SELECT revision, content_sha256 FROM local_messages
		WHERE account_id = ? AND id = ? AND source_revision = ?`, accountID, id, sourceRevision).Scan(&revision, &storedHash)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(ctx, `SELECT revision, json_extract(metadata_json, '$.content_sha256') FROM local_message_revisions
			WHERE account_id = ? AND id = ? AND json_extract(metadata_json, '$.source_revision') = ?
			ORDER BY revision DESC LIMIT 1`, accountID, id, sourceRevision).Scan(&revision, &storedHash)
	}
	switch {
	case err == sql.ErrNoRows:
		return "", 0, "", nil
	case err != nil:
		return "", 0, "", fmt.Errorf("localmail: look up source revision %s of %s: %w", sourceRevision, id, err)
	case storedHash == hash:
		return DeliveryReplay, revision, "", nil
	default:
		return DeliveryConflict, revision, fmt.Sprintf("source revision %s is already stored as revision %d with different content; nothing was changed", sourceRevision, revision), nil
	}
}

// Receipt is one delivery_receipts row.
type Receipt struct {
	ReceiptID         int64     `json:"receipt_id"`
	CredentialID      string    `json:"credential_id"`
	Source            string    `json:"source"`
	DeliveryID        string    `json:"delivery_id"`
	AccountID         string    `json:"account_id"`
	ProviderMessageID string    `json:"provider_message_id"`
	SourceRevision    string    `json:"source_revision"`
	ContentSHA256     string    `json:"content_sha256"`
	Status            string    `json:"status"`
	ID                string    `json:"id,omitempty"`
	Revision          int64     `json:"revision,omitempty"`
	Detail            string    `json:"detail,omitempty"`
	ReceivedAt        time.Time `json:"received_at"`
}

// Receipts reads the delivery journal after a receipt id, oldest first,
// optionally for one status.
func (s *Store) Receipts(ctx context.Context, after int64, status string, limit int) ([]Receipt, error) {
	if limit <= 0 || limit > MaxEventPage {
		limit = MaxEventPage
	}
	query := `SELECT receipt_id, credential_id, source, delivery_id, account_id, provider_message_id, source_revision,
		content_sha256, status, message_id, revision, detail, received_at FROM delivery_receipts WHERE receipt_id > ?`
	args := []any{after}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY receipt_id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("localmail: read receipts: %w", err)
	}
	defer rows.Close()
	out := []Receipt{}
	for rows.Next() {
		var r Receipt
		var receivedAt int64
		if err := rows.Scan(&r.ReceiptID, &r.CredentialID, &r.Source, &r.DeliveryID, &r.AccountID, &r.ProviderMessageID,
			&r.SourceRevision, &r.ContentSHA256, &r.Status, &r.ID, &r.Revision, &r.Detail, &receivedAt); err != nil {
			return nil, fmt.Errorf("localmail: scan receipt: %w", err)
		}
		r.ReceivedAt = time.UnixMilli(receivedAt).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
