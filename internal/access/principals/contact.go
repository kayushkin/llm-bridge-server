package principals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Resolving a requester to one id.
//
// Mail arrives with a sender address and nothing else. A ticket must point at
// its requester by id — an address is a name, and a person who mails from a
// phone, a work account and a web form is three names for one requester — so
// the address has to become an id exactly once, and every later mail from that
// address has to find the same row.
//
// That is what ResolveContact does. It is deliberately the only write path
// that looks a principal up by anything but its id: the caller has an address
// and no id, which is the one moment where a lookup by name is the correct
// thing to do rather than a shortcut.
//
// Matching is on the address, lowercased and trimmed, among active contacts.
// It never matches a human or a group: an employee who also writes in as a
// customer is deliberately two rows, because one of them can log in and the
// other must never be able to.

// ContactResolution is what ResolveContact answers: the contact, and whether
// this call is what created it. Created is not decoration — a caller that
// suddenly creates a contact per message has a normalization bug, and this is
// the field that shows it.
type ContactResolution struct {
	Contact *Principal `json:"contact"`
	Created bool       `json:"created"`
}

// ErrInvalidContact is a resolve request that cannot be answered.
var ErrInvalidContact = errors.New("invalid contact")

// normalizeContactEmail is the match key: an address is compared lowercased
// and trimmed, because mail systems treat it that way and a requester who
// types their own address in capitals is the same requester.
func normalizeContactEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ResolveContact finds the active contact with this address, or creates one.
//
// displayName is used only when creating: an existing contact keeps the name
// it has, because the name on a later mail is not more true than the one a
// person typed when the contact was made, and overwriting it would rewrite
// every ticket's requester display on every mail.
func (s *Store) ResolveContact(ctx context.Context, email, displayName string) (*ContactResolution, error) {
	address := normalizeContactEmail(email)
	if address == "" {
		return nil, fmt.Errorf("%w: email is required — it is what the address is matched on", ErrInvalidContact)
	}
	if !strings.Contains(address, "@") {
		return nil, fmt.Errorf("%w: %q is not an email address", ErrInvalidContact, email)
	}

	existing, err := s.contactByEmail(ctx, address)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &ContactResolution{Contact: existing, Created: false}, nil
	}

	name := strings.TrimSpace(displayName)
	if name == "" {
		// The address is the only name there is. Better than refusing: a
		// ticket with a requester nobody named is still a ticket, and the
		// name can be edited later.
		name = address
	}
	created, err := s.Create(ctx, &Principal{Kind: KindContact, DisplayName: name, Email: address})
	if err != nil {
		return nil, err
	}
	return &ContactResolution{Contact: created, Created: true}, nil
}

// contactByEmail returns the oldest active contact with this address, or nil.
//
// Oldest rather than newest: if two rows ever exist for one address — two
// mails resolved at the same instant, before the index below was added — the
// first one is the one earlier tickets already point at.
func (s *Store) contactByEmail(ctx context.Context, address string) (*Principal, error) {
	// lower() under the C collation folds ASCII letters only, as SQLite's
	// LOWER did; btrim with no characters trims spaces, as its TRIM did.
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT seq FROM principals WHERE kind = $1 AND disabled_at = 0 AND lower(btrim(email) COLLATE "C") = $2 ORDER BY seq ASC LIMIT 1`,
		KindContact, address).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, formatID(seq), false)
}
