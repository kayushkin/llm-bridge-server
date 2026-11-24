package peoplestore

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Every vocabulary here is served (GET /identity-kinds, GET
// /relationship-kinds) rather than read off the rows, so no caller builds a
// picker out of whatever values happen to exist.

const (
	IdentityKindEmail      = "email"
	IdentityKindPhone      = "phone"
	IdentityKindMatrixUser = "matrix_user"
	IdentityKindDiscord    = "discord"
	IdentityKindURL        = "url"
)

// IdentityKinds is every kind of handle a person can have.
var IdentityKinds = []IdentityKind{
	{Kind: IdentityKindEmail, Description: "An email address. Stored trimmed and lowercased."},
	{Kind: IdentityKindPhone, Description: "A phone number in international form. Stored as + and digits; a number without a leading + is refused, because its country cannot be known."},
	{Kind: IdentityKindMatrixUser, Description: "A Matrix user id as multichat sees it, e.g. @telegram_123456789:matrix.example.com: one bridged chat account on WhatsApp, Telegram, Signal, Messenger, Discord, Slack or Google Messages."},
	{Kind: IdentityKindDiscord, Description: "A Discord username or numeric user id. Stored trimmed."},
	{Kind: IdentityKindURL, Description: "A web address that is theirs: a profile, a site. Stored trimmed."},
}

func identityKindNames() []string {
	names := make([]string, 0, len(IdentityKinds))
	for _, kind := range IdentityKinds {
		names = append(names, kind.Kind)
	}
	return names
}

var matrixUserPattern = regexp.MustCompile(`^@[^\s:]+:[^\s:]+(:\d+)?$`)

// NormalizeIdentity checks the kind and puts the value in the one spelling
// UNIQUE(kind, value) compares, so "Ann@X.com " and "ann@x.com" are one handle.
func NormalizeIdentity(input IdentityInput) (IdentityInput, error) {
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	known := false
	for _, candidate := range IdentityKinds {
		if candidate.Kind == kind {
			known = true
			break
		}
	}
	if !known {
		return input, fmt.Errorf("%w: unknown identity kind %q: use one of %s", ErrInvalid, input.Kind, strings.Join(identityKindNames(), ", "))
	}
	value := strings.TrimSpace(input.Value)
	if value == "" {
		return input, fmt.Errorf("%w: identity value is required", ErrInvalid)
	}
	switch kind {
	case IdentityKindEmail:
		value = strings.ToLower(value)
		if at := strings.Index(value, "@"); at <= 0 || at == len(value)-1 || strings.ContainsAny(value, " \t") {
			return input, fmt.Errorf("%w: %q is not an email address", ErrInvalid, input.Value)
		}
	case IdentityKindPhone:
		if !strings.HasPrefix(value, "+") {
			return input, fmt.Errorf("%w: phone %q has no leading +: give it in international form, e.g. +12025550100", ErrInvalid, input.Value)
		}
		var digits strings.Builder
		for _, character := range value[1:] {
			switch {
			case character >= '0' && character <= '9':
				digits.WriteRune(character)
			case strings.ContainsRune(" -.()", character):
			default:
				return input, fmt.Errorf("%w: phone %q holds %q, which is neither a digit nor a separator", ErrInvalid, input.Value, character)
			}
		}
		if digits.Len() < 7 || digits.Len() > 15 {
			return input, fmt.Errorf("%w: phone %q has %d digits; an international number has 7 to 15", ErrInvalid, input.Value, digits.Len())
		}
		value = "+" + digits.String()
	case IdentityKindMatrixUser:
		if !matrixUserPattern.MatchString(value) {
			return input, fmt.Errorf("%w: %q is not a Matrix user id (@localpart:server)", ErrInvalid, input.Value)
		}
	}
	return IdentityInput{Kind: kind, Value: value, Label: strings.TrimSpace(input.Label)}, nil
}

// RelationshipKinds is every relationship a person can have to another, each
// with the kind it is from the other side.
var RelationshipKinds = []RelationshipKind{
	{Kind: "partner", Inverse: "partner", Description: "A romantic partner they are not married to."},
	{Kind: "spouse", Inverse: "spouse", Description: "A husband or wife."},
	{Kind: "parent", Inverse: "child", Description: "A mother or father."},
	{Kind: "child", Inverse: "parent", Description: "A son or daughter."},
	{Kind: "sibling", Inverse: "sibling", Description: "A brother or sister."},
	{Kind: "friend", Inverse: "friend", Description: "A friend."},
	{Kind: "coworker", Inverse: "coworker", Description: "Someone they work with or worked with."},
	{Kind: "other", Inverse: "other", Description: "Anything else; say what in the note."},
}

func relationshipKindFor(raw string) (RelationshipKind, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	names := make([]string, 0, len(RelationshipKinds))
	for _, kind := range RelationshipKinds {
		if kind.Kind == key {
			return kind, nil
		}
		names = append(names, kind.Kind)
	}
	return RelationshipKind{}, fmt.Errorf("%w: unknown relationship kind %q: use one of %s", ErrInvalid, raw, strings.Join(names, ", "))
}

// InverseRelationshipKind is what kind is when seen from the other person.
func InverseRelationshipKind(kind string) string {
	for _, candidate := range RelationshipKinds {
		if candidate.Kind == kind {
			return candidate.Inverse
		}
	}
	// Only kinds from RelationshipKinds are ever written, so a miss here is a
	// row this code did not write. Say so where it shows rather than guess.
	panic(fmt.Sprintf("relationship kind %q is not in RelationshipKinds", kind))
}

// Link sources: who attached a handle to a person, who refused a pair, who
// reviewed or claimed a handle. Served at GET /link-sources.
const (
	LinkSourceOperator    = "operator"
	LinkSourcePhoneNumber = "phone_number"
	LinkSourceAgent       = "agent"
	LinkSourceImport      = "import"
)

// LinkSources is every source of a link. Only a source with
// OverridesRefusals may link a pair the operator refused, or move a handle
// from one person to another; the rest get a 409.
var LinkSources = []LinkSource{
	{Source: LinkSourceOperator, OverridesRefusals: true, ReasonRequired: false,
		Description: "The operator, in dash's People page or multichat's person picker. Overrides a refusal and deletes it, and may move a handle from one person to another."},
	{Source: LinkSourcePhoneNumber, ReasonRequired: true,
		Description: "multichat's phone-number matcher: two bridge accounts that carry one phone number in the bridge databases. The reason names the number."},
	{Source: LinkSourceAgent, ReasonRequired: true,
		Description: "The contact review agent, when it would bet 9 to 1. The reason quotes the evidence."},
	{Source: LinkSourceImport, ReasonRequired: true,
		Description: "A one-off copy from another store whose record did not say who linked it; also every handle attached before people-store recorded a source."},
}

func linkSourceFor(raw, field string) (LinkSource, error) {
	names := make([]string, 0, len(LinkSources))
	for _, source := range LinkSources {
		if source.Source == raw {
			return source, nil
		}
		names = append(names, source.Source)
	}
	return LinkSource{}, fmt.Errorf("%w: %s %q is not a link source: use one of %s", ErrInvalid, field, raw, strings.Join(names, ", "))
}

// Suggestion sources and statuses.
const (
	// SuggestionSourceMultichat is ~/bin/people-import-multichat, an importer.
	SuggestionSourceMultichat = "multichat"
	// SuggestionSourceAgent is the contact review agent, a reviewer.
	SuggestionSourceAgent = "agent"
	// SuggestionSourceGoogleContacts is ~/bin/people-import-contacts, an
	// importer of a Google Contacts export (vCard files).
	SuggestionSourceGoogleContacts = "google_contacts"
	// SuggestionSourcePhoneMessages is journal-store's
	// journal-import-android-messages, an importer: a phone number the
	// operator texted with that no person has, named as the phone's own
	// contact list saved it.
	SuggestionSourcePhoneMessages = "phone_messages"

	SuggestionStatusProposed = "proposed"
	SuggestionStatusAccepted = "accepted"
	SuggestionStatusRejected = "rejected"
	// SuggestionStatusSuperseded is a suggestion every handle of which was
	// attached to a person some other way (a direct link, another
	// suggestion's accept) before anyone decided it.
	SuggestionStatusSuperseded = "superseded"
)

// SuggestionSources is every source that may propose suggestions.
var SuggestionSources = []string{SuggestionSourceMultichat, SuggestionSourceAgent, SuggestionSourceGoogleContacts, SuggestionSourcePhoneMessages}

// importerSuggestionSources propose a handle at most once, ever: a handle in
// any suggestion, whatever its status, is skipped. The other sources are
// reviewers: they skip only a handle in one of their own suggestions that is
// still proposed, must give a reason, and get a 409 for a handle the operator
// refused for the person the suggestion points at.
var importerSuggestionSources = []string{SuggestionSourceMultichat, SuggestionSourceGoogleContacts, SuggestionSourcePhoneMessages}

// SuggestionStatuses is every state a suggestion can be in.
var SuggestionStatuses = []string{SuggestionStatusProposed, SuggestionStatusAccepted, SuggestionStatusRejected, SuggestionStatusSuperseded}

// ReviewClaimSeconds is how long a review claim keeps a handle out of the
// pending queue.
const ReviewClaimSeconds = 30 * 60

// Orders GET /suggestions?sort= accepts.
const (
	// SuggestionSortCreated is oldest first, the order the importer proposed them.
	SuggestionSortCreated = "created"
	// SuggestionSortActivity is the people the operator talks to first: those
	// with a one-to-one chat, then the most recently active, then the most
	// messages, then oldest first.
	SuggestionSortActivity = "activity"
)

// SuggestionSorts is every order GET /suggestions?sort= accepts; the first is
// the default.
var SuggestionSorts = []string{SuggestionSortCreated, SuggestionSortActivity}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// Revision actions: what replaced the snapshot a revision holds.
const (
	RevisionActionUpdate         = "update"
	RevisionActionDelete         = "delete"
	RevisionActionRestore        = "restore"
	RevisionActionIdentityAdd    = "identity_add"
	RevisionActionIdentityRemove = "identity_remove"
)

var principalIDPattern = regexp.MustCompile(`^principal_\d+$`)

// normalizeBirthday accepts "", "YYYY-MM-DD", or "--MM-DD" for a birthday
// whose year is unknown, and refuses any date that does not exist.
func normalizeBirthday(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "--") {
		// 2000 is a leap year, so --02-29 is accepted.
		parsed, err := time.Parse("2006-01-02", "2000-"+value[2:])
		if err != nil || parsed.Format("--01-02") != value {
			return "", fmt.Errorf("%w: birthday %q is not a date: use YYYY-MM-DD, or --MM-DD when the year is unknown", ErrInvalid, raw)
		}
		return value, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return "", fmt.Errorf("%w: birthday %q is not a date: use YYYY-MM-DD, or --MM-DD when the year is unknown", ErrInvalid, raw)
	}
	return value, nil
}

func normalizeTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, fmt.Errorf("%w: a tag is empty", ErrInvalid)
		}
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out, nil
}
