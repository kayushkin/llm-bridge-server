package main

import (
	"strings"
	"testing"
)

// The schema is the only instruction the model gets about the SHAPE of its
// answer. Nothing in this process branches on it: it is serialized, sent, and
// acted on by a model on the far side, so it can only be wrong by disagreeing
// with something on THIS side. prompt_vocabulary_test.go holds the state enum
// and the title bound against the applier; the two tests here hold the rest.
//
// Ported 2026-09-25 from d9ba6aa5, which read the schema out of a forced
// tool_use block. Main now sends it as the oneshot request's `schema`, so the
// tool-name test has no subject and is not ported.
//
// Read off the wire rather than from reportSchema() directly: a test that calls
// the builder proves the builder agrees with itself.
func sentSchema(t *testing.T) map[string]any {
	t.Helper()
	body := captureRequest(t, "digest")
	schema, ok := body["schema"].(map[string]any)
	if !ok {
		t.Fatalf("no schema in the request: %v", body)
	}
	return schema
}

// trackable is the one field the classifier cannot do without: an absent
// trackable decodes to Go's zero value, false, which is indistinguishable from
// a deliberate "this session has no real work in it" — and that answer tags the
// session do-not-track and stops it being looked at again. So a reply that
// merely forgot the field silently switches the session off.
func TestTheSchemaRequiresTheOneFieldWhoseAbsenceIsNotDistinguishableFromFalse(t *testing.T) {
	schema := sentSchema(t)
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("no required list on the schema: %v", schema)
	}
	for _, r := range required {
		if r == "trackable" {
			return
		}
	}
	t.Fatalf("trackable is not required; an omitted trackable decodes as false and tags the "+
		"session do-not-track. required = %v", required)
}

// The existing-cards section of the digest exists so the model can dedupe
// against work already filed, and the schema's description is what turns being
// SHOWN the cards into being asked to answer on them. When the model answers
// on a subset, the rest keep the state they were first filed in — silently,
// because processSession only warns when it addresses NONE of them.
func TestTheSchemaAsksForEveryExistingCardBackAndNotJustSome(t *testing.T) {
	properties, _ := sentSchema(t)["properties"].(map[string]any)
	cards, ok := properties["existing_cards"].(map[string]any)
	if !ok {
		t.Fatalf("no existing_cards property on the schema: %v", properties)
	}
	description, _ := cards["description"].(string)
	if !strings.Contains(strings.ToLower(description), "every card") {
		t.Fatalf("the existing_cards description does not demand every card back, so a partial "+
			"answer is a legal one: %q", description)
	}
}
