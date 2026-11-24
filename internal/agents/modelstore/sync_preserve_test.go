package modelstore

import (
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

// storedRow is a row as a person has left it: every field carries a value that
// is distinctive, so a copy that silently does not happen shows up as the
// provider's value rather than as a zero that could have come from anywhere.
func storedRow() Model {
	return Model{
		ID:         "claude-fable-5-1",
		Provider:   "anthropic",
		Name:       "Claude Fable 5.1",
		ShortName:  "fable-5.1",
		Aliases:    []string{"fable", "best"},
		MaxTokens:  400000,
		InputCost:  3.25,
		OutputCost: 16.5,
		Enabled:    false,
		Priority:   5,
	}
}

// providerAnswerFor is what a sync run builds from the API before it looks at
// the store: every user-owned field holds the value the code would write if the
// carry-over were missing. No field agrees with storedRow, so no assertion below
// can pass by coincidence.
func providerAnswerFor(stored Model) Model {
	return Model{
		ID:         stored.ID,
		Provider:   stored.Provider,
		Name:       "Claude Fable 5.1 (renamed upstream)",
		ShortName:  "",
		Aliases:    nil,
		MaxTokens:  200000,
		InputCost:  0,
		OutputCost: 0,
		Enabled:    true,
		Priority:   100,
	}
}

// adoptedFields and providerOwnedFields are this file's declaration of who owns
// each column of Model. They are checked against Model itself (every field is in
// exactly one list) and against adoptUserSetFieldsFrom (the method copies exactly
// the adopted set), so neither can drift from the other in silence.
var (
	adoptedFields = map[string]bool{
		"Enabled":    true,
		"Priority":   true,
		"InputCost":  true,
		"OutputCost": true,
		"Aliases":    true,
		"ShortName":  true,
	}
	providerOwnedFields = map[string]bool{
		"ID":        true, // the join key; the same on both sides by construction
		"Provider":  true,
		"Name":      true,
		"MaxTokens": true,
	}
)

// TestAdoptUserSetFieldsCarriesEveryUserOwnedField pins the six copies one at a
// time. Deleting any single line from adoptUserSetFieldsFrom reddens exactly the
// subtest that names it and no other, so a failure says which field was dropped
// rather than only that something was.
func TestAdoptUserSetFieldsCarriesEveryUserOwnedField(t *testing.T) {
	stored := storedRow()

	cases := []struct {
		field string
		got   func(Model) any
		want  any
	}{
		{"Enabled", func(m Model) any { return m.Enabled }, stored.Enabled},
		{"Priority", func(m Model) any { return m.Priority }, stored.Priority},
		{"InputCost", func(m Model) any { return m.InputCost }, stored.InputCost},
		{"OutputCost", func(m Model) any { return m.OutputCost }, stored.OutputCost},
		{"Aliases", func(m Model) any { return m.Aliases }, stored.Aliases},
		{"ShortName", func(m Model) any { return m.ShortName }, stored.ShortName},
	}

	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			fresh := providerAnswerFor(stored)
			existing := stored
			fresh.adoptUserSetFieldsFrom(&existing)

			if got := c.got(fresh); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s was not carried over from the stored row:\n got %#v\nwant %#v",
					c.field, got, c.want)
			}
		})
	}
}

// TestAdoptUserSetFieldsLeavesProviderMetadataAlone is the other half of the
// contract and the reason the method is not simply an assignment of the whole
// struct. Anthropic and Google are the authority on the display name and the
// context window, and their call sites rely on this method not touching either.
func TestAdoptUserSetFieldsLeavesProviderMetadataAlone(t *testing.T) {
	stored := storedRow()
	fresh := providerAnswerFor(stored)
	fromProvider := fresh

	existing := stored
	fresh.adoptUserSetFieldsFrom(&existing)

	if fresh.Name != fromProvider.Name {
		t.Errorf("Name was overwritten from the stored row: got %q, want the provider's %q",
			fresh.Name, fromProvider.Name)
	}
	if fresh.MaxTokens != fromProvider.MaxTokens {
		t.Errorf("MaxTokens was overwritten from the stored row: got %d, want the provider's %d",
			fresh.MaxTokens, fromProvider.MaxTokens)
	}
}

// TestEveryModelFieldIsEitherAdoptedOrProviderOwned is the guard that outlives
// the six cases above. The defect this whole file is about is a column that
// exists in Model and in no carry-over list, so it survives a write and is erased
// by the next sync. A person adding a column will not think to add a case here —
// so this test enumerates Model by reflection and fails on any field it has not
// been told about, forcing the choice to be made and written down.
//
// To resolve a failure: add the field to adoptedFields if a person sets it, or
// to providerOwnedFields if the API answer is the authority on it. Do not add it
// to both, and do not delete this test to make it pass.
func TestEveryModelFieldIsEitherAdoptedOrProviderOwned(t *testing.T) {
	modelType := reflect.TypeOf(Model{})
	for i := 0; i < modelType.NumField(); i++ {
		name := modelType.Field(i).Name
		switch {
		case adoptedFields[name] && providerOwnedFields[name]:
			t.Errorf("Model.%s is listed as both adopted and provider-owned; it can only be one", name)
		case adoptedFields[name], providerOwnedFields[name]:
		default:
			t.Errorf("Model.%s is in neither list, so nothing decides whether a sync keeps it or "+
				"erases it. Add it to adoptedFields if a person sets it, or to providerOwnedFields "+
				"if the provider is the authority.", name)
		}
	}

	// The lists must also not outlive the struct: a field renamed or removed
	// leaves a stale entry that would keep this test green while covering nothing.
	for _, list := range []struct {
		name    string
		entries map[string]bool
	}{{"adoptedFields", adoptedFields}, {"providerOwnedFields", providerOwnedFields}} {
		for field := range list.entries {
			if _, ok := modelType.FieldByName(field); !ok {
				t.Errorf("%s names %q, which Model no longer has", list.name, field)
			}
		}
	}
}

// TestAdoptedFieldsMatchesWhatTheMethodActuallyCopies closes the gap the lists
// cannot close on their own: adoptedFields is a claim about
// adoptUserSetFieldsFrom, and without this nothing compares the two. Deleting a
// line from the method leaves the list still saying that field is adopted, and
// this is the test that notices.
func TestAdoptedFieldsMatchesWhatTheMethodActuallyCopies(t *testing.T) {
	stored := storedRow()
	fresh := providerAnswerFor(stored)
	fromProvider := fresh

	existing := stored
	fresh.adoptUserSetFieldsFrom(&existing)

	modelType := reflect.TypeOf(Model{})
	after := reflect.ValueOf(fresh)
	before := reflect.ValueOf(fromProvider)
	storedValue := reflect.ValueOf(stored)

	observed := map[string]bool{}
	for i := 0; i < modelType.NumField(); i++ {
		name := modelType.Field(i).Name
		if name == "ID" || name == "Provider" {
			continue // identical on both sides, so neither answer is evidence
		}
		tookStored := reflect.DeepEqual(after.Field(i).Interface(), storedValue.Field(i).Interface())
		keptProvider := reflect.DeepEqual(after.Field(i).Interface(), before.Field(i).Interface())
		if tookStored == keptProvider {
			t.Fatalf("Model.%s holds indistinguishable values in the stored row and the provider "+
				"answer, so this test cannot tell a copy from a no-op. Give the two fixtures "+
				"different values for it.", name)
		}
		if tookStored {
			observed[name] = true
		}
	}

	for name := range adoptedFields {
		if !observed[name] {
			t.Errorf("adoptedFields lists %q but adoptUserSetFieldsFrom does not copy it — "+
				"a sync will erase whatever a person set there", name)
		}
	}
	for name := range observed {
		if !adoptedFields[name] {
			t.Errorf("adoptUserSetFieldsFrom copies %q, which adoptedFields does not list — "+
				"either the copy or the list is wrong", name)
		}
	}
}
