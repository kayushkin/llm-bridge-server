package principalstore

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
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

func formatIdentityBindingID(seq int64) string { return fmt.Sprintf("identity_binding_%06d", seq) }

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

// requireActingHuman checks that an administrator id names an active human.
// The store has no auth, so it cannot check that this person is an
// administrator — the caller does that — but it can refuse an audit field
// that points at nobody.
func (s *Store) requireActingHuman(id, field string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: %s is required — the principal who made this change", ErrInvalidIdentityBinding, field)
	}
	actor, err := s.getRow(id)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %s %s is not a principal", ErrInvalidIdentityBinding, field, id)
	}
	if err != nil {
		return err
	}
	if actor.Kind != KindHuman || actor.DisabledAt != 0 {
		return fmt.Errorf("%w: %s %s must be an active human", ErrInvalidIdentityBinding, field, id)
	}
	return nil
}

const identityBindingColumns = `id, seq, principal_id, provider, tenant_id, subject, linked_at, linked_by, link_note, unlinked_at, unlinked_by, unlink_reason`

func scanIdentityBinding(scan func(...any) error) (*IdentityBinding, error) {
	b := &IdentityBinding{}
	if err := scan(&b.ID, &b.Seq, &b.PrincipalID, &b.Provider, &b.TenantID, &b.Subject,
		&b.LinkedAt, &b.LinkedBy, &b.LinkNote, &b.UnlinkedAt, &b.UnlinkedBy, &b.UnlinkReason); err != nil {
		return nil, err
	}
	return b, nil
}

// LinkIdentity binds an identity to a human principal. An identity that
// already has an active binding is ErrIdentityAlreadyBound, even to the same
// principal: a second link is a mistake to show, not a no-op to hide.
func (s *Store) LinkIdentity(principalID, provider, tenantID, subject, linkedBy, note string) (*IdentityBinding, error) {
	provider, tenantID, subject, err := normalizeIdentity(provider, tenantID, subject)
	if err != nil {
		return nil, err
	}
	target, err := s.getRow(principalID)
	if err != nil {
		return nil, err
	}
	if target.Kind != KindHuman {
		return nil, fmt.Errorf("%w: %s is a %s — only a human signs in", ErrInvalidIdentityBinding, principalID, target.Kind)
	}
	if target.DisabledAt != 0 {
		return nil, fmt.Errorf("%w: %s is disabled — enable it before giving it a sign-in", ErrInvalidIdentityBinding, principalID)
	}
	if err := s.requireActingHuman(linkedBy, "linked_by"); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	existing, err := activeBinding(tx, provider, tenantID, subject)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("%w: %s %s/%s is already %s, signing in as %s — unlink it first",
			ErrIdentityAlreadyBound, provider, tenantID, subject, existing.ID, existing.PrincipalID)
	}
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM identity_bindings`).Scan(&maxSeq); err != nil {
		return nil, err
	}
	b := &IdentityBinding{
		Seq: maxSeq.Int64 + 1, PrincipalID: principalID, Provider: provider, TenantID: tenantID, Subject: subject,
		LinkedAt: now(), LinkedBy: linkedBy, LinkNote: strings.TrimSpace(note),
	}
	b.ID = formatIdentityBindingID(b.Seq)
	if _, err := tx.Exec(`INSERT INTO identity_bindings (`+identityBindingColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Seq, b.PrincipalID, b.Provider, b.TenantID, b.Subject, b.LinkedAt, b.LinkedBy, b.LinkNote, 0, "", ""); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: identity_bindings.provider") {
			return nil, fmt.Errorf("%w: %s %s/%s was linked by another request at the same moment", ErrIdentityAlreadyBound, provider, tenantID, subject)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return b, nil
}

func activeBinding(tx *sql.Tx, provider, tenantID, subject string) (*IdentityBinding, error) {
	b, err := scanIdentityBinding(tx.QueryRow(
		`SELECT `+identityBindingColumns+` FROM identity_bindings WHERE provider = ? AND tenant_id = ? AND subject = ? AND unlinked_at = 0`,
		provider, tenantID, subject).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// UnlinkIdentity ends a binding. Idempotent: a second unlink answers the row
// as the first one left it, without moving the stamp or the reason.
func (s *Store) UnlinkIdentity(id, unlinkedBy, reason string) (*IdentityBinding, error) {
	if err := s.requireActingHuman(unlinkedBy, "unlinked_by"); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required — the next administrator needs to know why this sign-in stopped working", ErrInvalidIdentityBinding)
	}
	if _, err := s.db.Exec(
		`UPDATE identity_bindings SET unlinked_at = ?, unlinked_by = ?, unlink_reason = ? WHERE id = ? AND unlinked_at = 0`,
		now(), unlinkedBy, reason, id); err != nil {
		return nil, err
	}
	return s.GetIdentityBinding(id)
}

// GetIdentityBinding returns one binding, active or not.
func (s *Store) GetIdentityBinding(id string) (*IdentityBinding, error) {
	b, err := scanIdentityBinding(s.db.QueryRow(`SELECT `+identityBindingColumns+` FROM identity_bindings WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: identity binding %s", ErrNotFound, id)
	}
	return b, err
}

// ResolveIdentity finds the principal an identity signs in as. No active
// binding is ErrNotFound: the identity is unprovisioned.
func (s *Store) ResolveIdentity(provider, tenantID, subject string) (*IdentityBindingResolution, error) {
	provider, tenantID, subject, err := normalizeIdentity(provider, tenantID, subject)
	if err != nil {
		return nil, err
	}
	b, err := scanIdentityBinding(s.db.QueryRow(
		`SELECT `+identityBindingColumns+` FROM identity_bindings WHERE provider = ? AND tenant_id = ? AND subject = ? AND unlinked_at = 0`,
		provider, tenantID, subject).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no active binding for %s %s/%s — this identity is not provisioned", ErrNotFound, provider, tenantID, subject)
	}
	if err != nil {
		return nil, err
	}
	p, err := s.Get(b.PrincipalID, false)
	if err != nil {
		return nil, err
	}
	return &IdentityBindingResolution{Binding: b, Principal: p}, nil
}

// ListIdentityBindings returns bindings oldest first.
func (s *Store) ListIdentityBindings(f IdentityBindingFilter) ([]*IdentityBinding, error) {
	var where []string
	var args []any
	if f.PrincipalID != "" {
		where = append(where, "principal_id = ?")
		args = append(args, f.PrincipalID)
	}
	if f.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Provider)))
	}
	if f.TenantID != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.TenantID)))
	}
	if !f.IncludeUnlinked {
		where = append(where, "unlinked_at = 0")
	}
	query := `SELECT ` + identityBindingColumns + ` FROM identity_bindings`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(query+" ORDER BY seq ASC", args...)
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
