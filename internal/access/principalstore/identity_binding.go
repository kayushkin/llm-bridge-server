package principalstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// Binding a sign-in identity to a principal.
//
// A company login proves "this is object X in tenant Y" and nothing else. The
// principal it signs in as has to be named by a person, once, and found again
// by that exact pair every time after. So a binding is the triple
// (provider, tenant_id, subject) pointing at one human principal, written by
// an administrator, never by the login itself.
//
// It is deliberately not matched on email or display name. An address is a
// name: two rows may share one, a person's address changes when they marry,
// and an attacker who can register an address in another tenant would
// otherwise sign in as whoever already has it. A login whose identity has no
// binding is unprovisioned, and the login path says so instead of guessing.
//
// No row is ever deleted. Unlinking stamps unlinked_at, unlinked_by and the
// reason, so who could sign in as whom, and who decided it, stays answerable.
// Linking the same identity again after an unlink writes a new row.

// IdentityProviderEntra is Microsoft Entra ID. Its tenant_id is the `tid`
// claim and its subject is the `oid` claim — the object id, which is stable
// for a user across every application in the tenant. Not `sub`, which Entra
// makes different for each application.
const IdentityProviderEntra = "entra"

// IdentityProviderDiscord is Discord. It has no tenant, so tenant_id is
// empty, and its subject is the Discord user id (a decimal snowflake), never
// the username, which a person can change.
const IdentityProviderDiscord = "discord"

// IdentityProviders is every provider a binding can name, served at
// GET /identity-providers so no caller hardcodes the list.
var IdentityProviders = []string{IdentityProviderEntra, IdentityProviderDiscord}

// ErrInvalidIdentityBinding is a link or unlink request that cannot be done
// as asked.
var ErrInvalidIdentityBinding = errors.New("invalid identity binding")

// ErrIdentityAlreadyBound is a link of an identity that already has an active
// binding. The caller must unlink the old one first, as its own act.
var ErrIdentityAlreadyBound = errors.New("identity already bound")

// IdentityBinding is one sign-in identity linked to one human principal.
type IdentityBinding struct {
	ID           string `json:"id"`
	Seq          int64  `json:"seq"`
	PrincipalID  string `json:"principal_id"`
	Provider     string `json:"provider"`
	TenantID     string `json:"tenant_id"`
	Subject      string `json:"subject"`
	LinkedAt     int64  `json:"linked_at"`
	LinkedBy     string `json:"linked_by"`
	LinkNote     string `json:"link_note"`
	UnlinkedAt   int64  `json:"unlinked_at"`
	UnlinkedBy   string `json:"unlinked_by"`
	UnlinkReason string `json:"unlink_reason"`
}

// IdentityBindingResolution is what GET /identity-bindings/resolve answers:
// the active binding and the principal it signs in as. The principal comes
// back even when disabled, with disabled_at set, so the login path can say
// "disabled" rather than "unprovisioned".
type IdentityBindingResolution struct {
	Binding   *IdentityBinding `json:"binding"`
	Principal *Principal       `json:"principal"`
}

// IdentityBindingFilter selects bindings. The zero value lists every active one.
type IdentityBindingFilter struct {
	PrincipalID     string
	Provider        string
	TenantID        string
	IncludeUnlinked bool
}

func formatIdentityBindingID(seq int64) string {
	return postgresstorage.FormatRecordID(IdentityBindingIDPrefix, seq)
}

// entraIdentifierPattern is the shape of an Entra tenant id and object id: a
// GUID. Checking it catches the mistake that matters most — an email address
// or a `sub` claim pasted where the object id belongs.
var entraIdentifierPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// discordUserIDPattern is the shape of a Discord user id: a snowflake, 17 to
// 20 decimal digits today. Checking it catches a username pasted where the id
// belongs.
var discordUserIDPattern = regexp.MustCompile(`^[0-9]{15,21}$`)

// normalizeIdentity checks the provider and lowercases and trims the tenant
// and subject, so a GUID typed in capitals finds the row a login writes in
// lowercase.
func normalizeIdentity(provider, tenantID, subject string) (string, string, string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	tenantID = strings.ToLower(strings.TrimSpace(tenantID))
	subject = strings.ToLower(strings.TrimSpace(subject))
	switch provider {
	case IdentityProviderEntra:
		if !entraIdentifierPattern.MatchString(tenantID) {
			return "", "", "", fmt.Errorf("%w: tenant_id %q is not an Entra tenant id (a GUID, the tid claim)", ErrInvalidIdentityBinding, tenantID)
		}
		if !entraIdentifierPattern.MatchString(subject) {
			return "", "", "", fmt.Errorf("%w: subject %q is not an Entra object id (a GUID, the oid claim — not the email and not sub)", ErrInvalidIdentityBinding, subject)
		}
	case IdentityProviderDiscord:
		if tenantID != "" {
			return "", "", "", fmt.Errorf("%w: tenant_id must be empty for discord, which has no tenant; got %q", ErrInvalidIdentityBinding, tenantID)
		}
		if !discordUserIDPattern.MatchString(subject) {
			return "", "", "", fmt.Errorf("%w: subject %q is not a Discord user id (a decimal snowflake — not the username, which can change)", ErrInvalidIdentityBinding, subject)
		}
	default:
		return "", "", "", fmt.Errorf("%w: unknown provider %q: use one of %s",
			ErrInvalidIdentityBinding, provider, strings.Join(IdentityProviders, ", "))
	}
	return provider, tenantID, subject, nil
}

// requireActingHuman checks that an administrator id names an active human,
// and returns its number. The store cannot check that this person is an
// administrator — the caller does that — but it can refuse an audit field
// that points at nobody.
func (s *Store) requireActingHuman(ctx context.Context, id, field string) (int64, error) {
	if strings.TrimSpace(id) == "" {
		return 0, fmt.Errorf("%w: %s is required — the principal who made this change", ErrInvalidIdentityBinding, field)
	}
	actor, err := s.getRow(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return 0, fmt.Errorf("%w: %s %s is not a principal", ErrInvalidIdentityBinding, field, id)
	}
	if err != nil {
		return 0, err
	}
	if actor.Kind != KindHuman || actor.DisabledAt != 0 {
		return 0, fmt.Errorf("%w: %s %s must be an active human", ErrInvalidIdentityBinding, field, id)
	}
	return actor.Seq, nil
}

const identityBindingColumns = `seq, principal_seq, provider, tenant_id, subject, linked_at, linked_by_seq, link_note, unlinked_at, unlinked_by_seq, unlink_reason`

func scanIdentityBinding(scan func(...any) error) (*IdentityBinding, error) {
	b := &IdentityBinding{}
	var principalSeq, linkedBySeq int64
	var unlinkedBySeq sql.NullInt64
	if err := scan(&b.Seq, &principalSeq, &b.Provider, &b.TenantID, &b.Subject,
		&b.LinkedAt, &linkedBySeq, &b.LinkNote, &b.UnlinkedAt, &unlinkedBySeq, &b.UnlinkReason); err != nil {
		return nil, err
	}
	b.ID = formatIdentityBindingID(b.Seq)
	b.PrincipalID = formatID(principalSeq)
	b.LinkedBy = formatID(linkedBySeq)
	if unlinkedBySeq.Valid {
		b.UnlinkedBy = formatID(unlinkedBySeq.Int64)
	}
	return b, nil
}

// activeIdentityIndex is the partial unique index that holds one active
// binding per identity when two links race.
const activeIdentityIndex = "identity_bindings_active_identity_index"

// LinkIdentity binds an identity to a human principal. An identity that
// already has an active binding is ErrIdentityAlreadyBound, even to the same
// principal: a second link is a mistake to show, not a no-op to hide.
func (s *Store) LinkIdentity(ctx context.Context, principalID, provider, tenantID, subject, linkedBy, note string) (*IdentityBinding, error) {
	provider, tenantID, subject, err := normalizeIdentity(provider, tenantID, subject)
	if err != nil {
		return nil, err
	}
	target, err := s.getRow(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if target.Kind != KindHuman {
		return nil, fmt.Errorf("%w: %s is a %s — only a human signs in", ErrInvalidIdentityBinding, principalID, target.Kind)
	}
	if target.DisabledAt != 0 {
		return nil, fmt.Errorf("%w: %s is disabled — enable it before giving it a sign-in", ErrInvalidIdentityBinding, principalID)
	}
	linkedBySeq, err := s.requireActingHuman(ctx, linkedBy, "linked_by")
	if err != nil {
		return nil, err
	}

	var linked *IdentityBinding
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		existing, err := activeBinding(ctx, tx, provider, tenantID, subject)
		if err != nil {
			return err
		}
		if existing != nil {
			return fmt.Errorf("%w: %s %s/%s is already %s, signing in as %s — unlink it first",
				ErrIdentityAlreadyBound, provider, tenantID, subject, existing.ID, existing.PrincipalID)
		}
		var seq int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO identity_bindings (principal_seq, provider, tenant_id, subject, linked_at, linked_by_seq, link_note)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING seq`,
			target.Seq, provider, tenantID, subject, now(), linkedBySeq, strings.TrimSpace(note)).Scan(&seq); err != nil {
			var postgresError *pgconn.PgError
			if errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == activeIdentityIndex {
				return fmt.Errorf("%w: %s %s/%s was linked by another request at the same moment", ErrIdentityAlreadyBound, provider, tenantID, subject)
			}
			return err
		}
		linked, err = scanIdentityBinding(tx.QueryRowContext(ctx, `SELECT `+identityBindingColumns+` FROM identity_bindings WHERE seq = $1`, seq).Scan)
		return err
	})
	if err != nil {
		return nil, err
	}
	return linked, nil
}

func activeBinding(ctx context.Context, tx *sql.Tx, provider, tenantID, subject string) (*IdentityBinding, error) {
	b, err := scanIdentityBinding(tx.QueryRowContext(ctx,
		`SELECT `+identityBindingColumns+` FROM identity_bindings WHERE provider = $1 AND tenant_id = $2 AND subject = $3 AND unlinked_at = 0`,
		provider, tenantID, subject).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// UnlinkIdentity ends a binding. Idempotent: a second unlink answers the row
// as the first one left it, without moving the stamp or the reason.
func (s *Store) UnlinkIdentity(ctx context.Context, id, unlinkedBy, reason string) (*IdentityBinding, error) {
	unlinkedBySeq, err := s.requireActingHuman(ctx, unlinkedBy, "unlinked_by")
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required — the next administrator needs to know why this sign-in stopped working", ErrInvalidIdentityBinding)
	}
	seq, ok := numberOfID(IdentityBindingIDPrefix, id)
	if !ok {
		return nil, fmt.Errorf("%w: identity binding %s", ErrNotFound, id)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE identity_bindings SET unlinked_at = $1, unlinked_by_seq = $2, unlink_reason = $3 WHERE seq = $4 AND unlinked_at = 0`,
		now(), unlinkedBySeq, reason, seq); err != nil {
		return nil, err
	}
	return s.GetIdentityBinding(ctx, id)
}

// GetIdentityBinding returns one binding, active or not.
func (s *Store) GetIdentityBinding(ctx context.Context, id string) (*IdentityBinding, error) {
	seq, ok := numberOfID(IdentityBindingIDPrefix, id)
	if !ok {
		return nil, fmt.Errorf("%w: identity binding %s", ErrNotFound, id)
	}
	b, err := scanIdentityBinding(s.db.QueryRowContext(ctx, `SELECT `+identityBindingColumns+` FROM identity_bindings WHERE seq = $1`, seq).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: identity binding %s", ErrNotFound, id)
	}
	return b, err
}

// ResolveIdentity finds the principal an identity signs in as. No active
// binding is ErrNotFound: the identity is unprovisioned.
func (s *Store) ResolveIdentity(ctx context.Context, provider, tenantID, subject string) (*IdentityBindingResolution, error) {
	provider, tenantID, subject, err := normalizeIdentity(provider, tenantID, subject)
	if err != nil {
		return nil, err
	}
	b, err := scanIdentityBinding(s.db.QueryRowContext(ctx,
		`SELECT `+identityBindingColumns+` FROM identity_bindings WHERE provider = $1 AND tenant_id = $2 AND subject = $3 AND unlinked_at = 0`,
		provider, tenantID, subject).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no active binding for %s %s/%s — this identity is not provisioned", ErrNotFound, provider, tenantID, subject)
	}
	if err != nil {
		return nil, err
	}
	p, err := s.Get(ctx, b.PrincipalID, false)
	if err != nil {
		return nil, err
	}
	return &IdentityBindingResolution{Binding: b, Principal: p}, nil
}

// ListIdentityBindings returns bindings oldest first. A principal_id filter
// that is not a principal id matches nothing, as it did when ids were text.
func (s *Store) ListIdentityBindings(ctx context.Context, f IdentityBindingFilter) ([]*IdentityBinding, error) {
	var where []string
	var args []any
	argument := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.PrincipalID != "" {
		seq, ok := principalNumberOf(f.PrincipalID)
		if !ok {
			return []*IdentityBinding{}, nil
		}
		where = append(where, "principal_seq = "+argument(seq))
	}
	if f.Provider != "" {
		where = append(where, "provider = "+argument(strings.ToLower(strings.TrimSpace(f.Provider))))
	}
	if f.TenantID != "" {
		where = append(where, "tenant_id = "+argument(strings.ToLower(strings.TrimSpace(f.TenantID))))
	}
	if !f.IncludeUnlinked {
		where = append(where, "unlinked_at = 0")
	}
	query := `SELECT ` + identityBindingColumns + ` FROM identity_bindings`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY seq ASC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := []*IdentityBinding{}
	for rows.Next() {
		b, err := scanIdentityBinding(rows.Scan)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}
