package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	peoplestore "github.com/kayushkin/people-store"
)

// contactPhone is one TEL as the card wrote it and, when it could be put in
// international form, as people-store will store it.
type contactPhone struct {
	Raw   string `json:"raw"`
	E164  string `json:"e164,omitempty"`
	Label string `json:"label,omitempty"`
	// Skipped says why the number has no E164 form, when it has none.
	Skipped string `json:"skipped,omitempty"`
}

// contactEmail is one EMAIL, lowercased in Address.
type contactEmail struct {
	Raw     string `json:"raw"`
	Address string `json:"address"`
	Label   string `json:"label,omitempty"`
}

// contact is one card, or several cards for one person merged.
type contact struct {
	FormattedName string
	// StructuredName is "Given Family" from N, when the card has N.
	StructuredName string
	Nicknames      []string
	Organization   string
	Birthday       string // "", "YYYY-MM-DD" or "--MM-DD"
	BirthdayRaw    string
	Note           string
	Categories     []string
	Starred        bool
	HasPhoto       bool
	Phones         []contactPhone
	Emails         []contactEmail
	// Files are the export files the card was read from, relative to the
	// export directory.
	Files []string
}

// starredCategory is the CATEGORIES value Google gives a contact starred on
// Android; ownContactsCategory the one it gives every contact the owner saved.
const (
	starredCategory     = "starred"
	ownContactsCategory = "myContacts"
)

// displayName is the name a suggestion carries: FN, else the name from N,
// else a nickname, else the organization.
func (c *contact) displayName() string {
	for _, name := range append([]string{c.FormattedName, c.StructuredName}, append(c.Nicknames, c.Organization)...) {
		if strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// names are every name the contact goes by, for matching against persons.
func (c *contact) names() []string {
	var names []string
	for _, name := range append([]string{c.FormattedName, c.StructuredName}, c.Nicknames...) {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	return names
}

// handles are the contact's phones and emails as people-store identities.
// A number a card lists twice, in two spellings, is one handle.
func (c *contact) handles() []peoplestore.IdentityInput {
	var handles []peoplestore.IdentityInput
	seen := map[string]bool{}
	add := func(handle peoplestore.IdentityInput) {
		if key := handle.Kind + "\x00" + handle.Value; !seen[key] {
			seen[key] = true
			handles = append(handles, handle)
		}
	}
	for _, phone := range c.Phones {
		if phone.E164 != "" {
			add(peoplestore.IdentityInput{Kind: peoplestore.IdentityKindPhone, Value: phone.E164, Label: phone.Label})
		}
	}
	for _, email := range c.Emails {
		add(peoplestore.IdentityInput{Kind: peoplestore.IdentityKindEmail, Value: email.Address, Label: email.Label})
	}
	return handles
}

// isOtherContact says whether this is one of Google's "Other contacts": an
// address Gmail kept because mail went to or came from it, which the owner
// never saved. Those carry no CATEGORIES at all and no phone number; every
// contact the owner saved carries the myContacts category, and a phone's own
// device contacts carry a number.
func (c *contact) isOtherContact() bool {
	return len(c.Categories) == 0 && len(c.Phones) == 0
}

// contactFromCard reads the properties this importer uses from one card.
func contactFromCard(card vcard, file string, phoneRule phoneRule) contact {
	result := contact{Files: []string{file}}
	labels := map[string]string{} // group → X-ABLabel, Apple's and Google's label for a grouped property
	for _, property := range card.Properties {
		if property.Name == "X-ABLABEL" && property.Group != "" {
			labels[property.Group] = strings.TrimSpace(textValue(property.Value))
		}
	}
	labelOf := func(property vcardProperty) string {
		if label := labels[property.Group]; label != "" && property.Group != "" {
			return label
		}
		var types []string
		for _, kind := range property.Params["TYPE"] {
			kind = strings.ToLower(strings.TrimSpace(kind))
			if kind != "" && kind != "internet" && kind != "pref" && kind != "voice" {
				types = append(types, kind)
			}
		}
		return strings.Join(types, ",")
	}
	for _, property := range card.Properties {
		switch property.Name {
		case "FN":
			result.FormattedName = strings.TrimSpace(textValue(property.Value))
		case "N":
			parts := structuredValue(property.Value, ';')
			var given, family string
			if len(parts) > 0 {
				family = strings.TrimSpace(parts[0])
			}
			if len(parts) > 1 {
				given = strings.TrimSpace(parts[1])
			}
			result.StructuredName = strings.TrimSpace(given + " " + family)
		case "NICKNAME":
			for _, nickname := range structuredValue(property.Value, ',') {
				if nickname = strings.TrimSpace(nickname); nickname != "" {
					result.Nicknames = append(result.Nicknames, nickname)
				}
			}
		case "ORG":
			var parts []string
			for _, part := range structuredValue(property.Value, ';') {
				if part = strings.TrimSpace(part); part != "" {
					parts = append(parts, part)
				}
			}
			result.Organization = strings.Join(parts, ", ")
		case "BDAY":
			result.BirthdayRaw = strings.TrimSpace(property.Value)
			result.Birthday = normalizeBirthday(result.BirthdayRaw)
		case "NOTE":
			result.Note = strings.TrimSpace(textValue(property.Value))
		case "CATEGORIES":
			for _, category := range structuredValue(property.Value, ',') {
				if category = strings.TrimSpace(category); category != "" {
					result.Categories = append(result.Categories, category)
					if strings.EqualFold(category, starredCategory) {
						result.Starred = true
					}
				}
			}
		case "X-ANDROID-STARRED", "X-STARRED":
			if value := strings.TrimSpace(property.Value); value == "1" || strings.EqualFold(value, "true") {
				result.Starred = true
			}
		case "PHOTO":
			result.HasPhoto = true
		case "TEL":
			raw := strings.TrimSpace(textValue(property.Value))
			e164, skipped := phoneRule.normalize(raw)
			result.Phones = append(result.Phones, contactPhone{Raw: raw, E164: e164, Label: labelOf(property), Skipped: skipped})
		case "EMAIL":
			raw := strings.TrimSpace(textValue(property.Value))
			address := strings.ToLower(strings.TrimPrefix(raw, "mailto:"))
			if at := strings.Index(address, "@"); at <= 0 || at == len(address)-1 || strings.ContainsAny(address, " \t") {
				continue
			}
			result.Emails = append(result.Emails, contactEmail{Raw: raw, Address: address, Label: labelOf(property)})
		}
	}
	return result
}

// normalizeBirthday turns the BDAY forms vCard 3 and 4 write into
// people-store's: "YYYY-MM-DD", or "--MM-DD" when the year is unknown. A value
// it cannot read is "", and the raw value travels in the source detail.
func normalizeBirthday(raw string) string {
	value := raw
	if index := strings.IndexAny(value, "T"); index >= 0 {
		value = value[:index]
	}
	if strings.HasPrefix(value, "--") {
		digits := strings.ReplaceAll(value[2:], "-", "")
		if len(digits) != 4 {
			return ""
		}
		candidate := "--" + digits[:2] + "-" + digits[2:]
		if parsed, err := time.Parse("2006-01-02", "2000-"+candidate[2:]); err != nil || parsed.Format("--01-02") != candidate {
			return ""
		}
		return candidate
	}
	digits := strings.ReplaceAll(value, "-", "")
	if len(digits) != 8 {
		return ""
	}
	candidate := digits[:4] + "-" + digits[4:6] + "-" + digits[6:]
	if parsed, err := time.Parse("2006-01-02", candidate); err != nil || parsed.Format("2006-01-02") != candidate {
		return ""
	}
	return candidate
}

// phoneRule decides which numbers become E.164.
type phoneRule struct {
	// NorthAmerican says that a number written without a country code is a
	// North American Numbering Plan number, as on a phone sold and used in the
	// US or Canada. Without it such numbers are skipped: their country cannot
	// be known.
	NorthAmerican bool
}

// normalize returns the number in E.164 form, or "" and why not.
func (rule phoneRule) normalize(raw string) (e164, skipped string) {
	value := strings.TrimSpace(strings.TrimPrefix(raw, "tel:"))
	var digits strings.Builder
	for index, character := range value {
		switch {
		case character >= '0' && character <= '9':
			digits.WriteRune(character)
		case character == '+' && index == 0:
		case strings.ContainsRune(" -.()/", character):
		default:
			return "", fmt.Sprintf("holds %q: a service code or an extension, not a number", character)
		}
	}
	number := digits.String()
	if strings.HasPrefix(value, "+") {
		if len(number) < 7 || len(number) > 15 {
			return "", fmt.Sprintf("%d digits; an international number has 7 to 15", len(number))
		}
		return "+" + number, ""
	}
	if !rule.NorthAmerican {
		return "", "no country code"
	}
	if len(number) == 11 && number[0] == '1' {
		number = number[1:]
	}
	if len(number) != 10 {
		return "", fmt.Sprintf("no country code, and %d digits is not a North American number", len(digits.String()))
	}
	// NANP: the area code and the exchange each start with 2 to 9.
	if number[0] < '2' || number[3] < '2' {
		return "", "no country code, and not a valid North American number"
	}
	return "+1" + number, ""
}

// nameTokens lowercases a name and splits it into words, dropping
// punctuation and single-letter initials, so "Smith, Alice B." and
// "alice smith" give the same words. A lone digit stays: "Andrew 2" is
// someone the owner told apart from another Andrew.
func nameTokens(name string) []string {
	var tokens []string
	for _, word := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if runes := []rune(word); len(runes) > 1 || unicode.IsDigit(runes[0]) {
			tokens = append(tokens, word)
		}
	}
	return tokens
}

func tokenKey(tokens []string) string {
	sorted := append([]string(nil), tokens...)
	sort.Strings(sorted)
	return strings.Join(sorted, " ")
}

// namesPlausiblyMatch says whether a contact name and a person's name could
// be one person: the same words in any order, or, when both have two words
// or more, the same first and last word with something different between
// ("Alice Smith" and "Alice B. Jones Smith"). A one-word name matches only
// the same one word, so "Mom" matches a person called "Mom" and nobody else.
// This is only ever a hint in a suggestion; nothing links on it.
func namesPlausiblyMatch(contactName, personName string) bool {
	contactTokens, personTokens := nameTokens(contactName), nameTokens(personName)
	if len(contactTokens) == 0 || len(personTokens) == 0 {
		return false
	}
	if tokenKey(contactTokens) == tokenKey(personTokens) {
		return true
	}
	if len(contactTokens) < 2 || len(personTokens) < 2 {
		return false
	}
	return contactTokens[0] == personTokens[0] && contactTokens[len(contactTokens)-1] == personTokens[len(personTokens)-1]
}

// mergeContacts folds cards that are one person into one contact: the same
// name (by nameTokens, in any order) and at least one shared phone or email.
// One export lists a saved contact in its "All Contacts" file and again in
// every label's file. Cards with no handle at all merge on the name alone:
// nothing is proposed for them, and they are counted once. Cards with different names stay apart even when they
// share a number, since a household's landline is several people; the store
// gives each handle to the first suggestion that names it.
func mergeContacts(contacts []contact) []contact {
	parent := make([]int, len(contacts))
	for index := range parent {
		parent[index] = index
	}
	var find func(int) int
	find = func(index int) int {
		if parent[index] != index {
			parent[index] = find(parent[index])
		}
		return parent[index]
	}
	owner := map[string]int{} // name key + handle → first card that has it
	for index := range contacts {
		nameKey := tokenKey(nameTokens(contacts[index].displayName()))
		handles := contacts[index].handles()
		if len(handles) == 0 && nameKey != "" {
			handles = []peoplestore.IdentityInput{{Kind: "no handle"}}
		}
		for _, handle := range handles {
			key := nameKey + "\x00" + handle.Kind + "\x00" + handle.Value
			if first, seen := owner[key]; seen {
				parent[find(index)] = find(first)
			} else {
				owner[key] = index
			}
		}
	}
	var order []int
	merged := map[int]*contact{}
	for index := range contacts {
		root := find(index)
		target, exists := merged[root]
		if !exists {
			copied := contacts[index]
			merged[root] = &copied
			order = append(order, root)
			continue
		}
		mergeInto(target, contacts[index])
	}
	result := make([]contact, 0, len(order))
	for _, root := range order {
		result = append(result, *merged[root])
	}
	return result
}

func mergeInto(target *contact, other contact) {
	fillIn := func(field *string, value string) {
		if *field == "" {
			*field = value
		}
	}
	fillIn(&target.FormattedName, other.FormattedName)
	fillIn(&target.StructuredName, other.StructuredName)
	fillIn(&target.Organization, other.Organization)
	fillIn(&target.Birthday, other.Birthday)
	fillIn(&target.BirthdayRaw, other.BirthdayRaw)
	fillIn(&target.Note, other.Note)
	target.Nicknames = appendNew(target.Nicknames, other.Nicknames...)
	target.Categories = appendNew(target.Categories, other.Categories...)
	target.Files = appendNew(target.Files, other.Files...)
	target.Starred = target.Starred || other.Starred
	target.HasPhoto = target.HasPhoto || other.HasPhoto
	for _, phone := range other.Phones {
		known := false
		for _, existing := range target.Phones {
			if existing.Raw == phone.Raw || (phone.E164 != "" && existing.E164 == phone.E164) {
				known = true
				break
			}
		}
		if !known {
			target.Phones = append(target.Phones, phone)
		}
	}
	for _, email := range other.Emails {
		known := false
		for _, existing := range target.Emails {
			if existing.Address == email.Address {
				known = true
				break
			}
		}
		if !known {
			target.Emails = append(target.Emails, email)
		}
	}
}

func appendNew(values []string, more ...string) []string {
	for _, value := range more {
		found := false
		for _, existing := range values {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}
