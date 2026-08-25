package main

import (
	"strings"

	"github.com/slava/scheduler/internal/demoteam"
)

// The people this mailbox hears from.
//
// A fixed cast rather than generated names, for two reasons. Sender affinity is
// something the classifier LEARNS — a sender whose mail always lands on one
// card gets routed there with no model call at all — and that can only be
// exercised if the same address writes more than once. And a reply has to come
// from somebody already in the conversation, which needs a roster to draw from.
//
// Every address is under .example, which RFC 2606 reserves precisely so that
// test data cannot reach a real mailbox.

type person struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Email string `json:"email"`
	// Role is given to the model so the voice matches the sender: a product
	// manager asking for work does not write like a monitoring bot.
	//
	// It is deliberately NOT serialized into the corpus. It is an input to the
	// prompt, not a property of the message, and writing it beside every sender
	// would put twenty copies of it in the fixture waiting to drift away from
	// the cast it was copied from. The key is what the corpus records; the role
	// is looked up from here.
	Role string `json:"-"`
	// Slug is the agent-store identity behind this sender, empty for the machine
	// senders. It is what lets a message be attributed to somebody the board can
	// actually resolve, rather than to a display name.
	Slug string `json:"slug,omitempty"`
}

// owner is the mailbox all of this arrives in.
var owner = person{
	Key: "owner", Name: "Vlad", Email: "vlad@northwind-eng.example",
	Role: "the engineer who owns this mailbox",
}

// machineSenders are the addresses that belong to no one. They send mail and
// hold no identity: a build robot is not a member of the team, it is a thing
// the team's tooling does, and giving it an agent-store row would put a
// permission subject where there is no actor.
var machineSenders = []person{
	{Key: "ops", Name: "Customer Ops", Email: "ops@northwind-support.example",
		Role: "a shared customer-operations mailbox; reports customer-visible problems with order numbers"},
	{Key: "security", Name: "Security", Email: "security@northwind-eng.example",
		Role: "the security team; sends deadlines tied to audits and rotations"},
	{Key: "vendor", Name: "Fulfilment Partner Support", Email: "support@fulfilment-partner.example",
		Role: "an external vendor's support desk; slow, formal, quotes ticket numbers"},
	{Key: "ci", Name: "CI", Email: "ci-bot@northwind-eng.example",
		Role: "an automated build robot; writes identical terse digests with numbers in them, never prose"},
	{Key: "portal", Name: "Vendor Portal", Email: "no-reply@vendor-portal.example",
		Role: "an automated no-reply notifier whose messages are vague and hard to act on"},
}

// cast is the machine senders plus every roster member who sends mail. Built
// once at start-up from demoteam.Roster, so the people who write email and the
// identities that do work are the same list read two ways — a colleague cannot
// appear in the mailbox without existing on the board.
var cast = buildCast()

func buildCast() []person {
	out := append([]person(nil), machineSenders...)
	for _, m := range demoteam.Roster {
		if !m.SendsMail() {
			continue
		}
		out = append(out, person{
			Key:   strings.TrimPrefix(m.Slug, "northwind-"),
			Name:  m.Name,
			Email: m.Email,
			Role:  m.Voice,
			Slug:  m.Slug,
		})
	}
	return out
}

func personByKey(key string) person {
	for _, p := range cast {
		if p.Key == key {
			return p
		}
	}
	// A plan that names a sender who does not exist is a bug in the planner, not
	// something to paper over with a default.
	panic("synthetic-mail: unknown sender key " + key)
}
