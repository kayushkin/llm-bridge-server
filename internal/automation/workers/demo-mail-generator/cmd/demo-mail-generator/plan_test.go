package main

import (
	"math/rand"
	"testing"
)

// Every planned sender must have an address: mailstack rejects a message
// whose from.email is empty, and the roster's agents have none.
func TestPlannedSendersAllHaveMail(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for _, m := range planMessages(500, random) {
		if !m.From.SendsMail() {
			t.Fatalf("planned %s from %q (%s), who has no email", m.Key, m.From.Name, m.From.Slug)
		}
	}
}
