package main

import (
	"testing"

	"github.com/slava/scheduler/internal/demoteam"
)

// Every colleague who writes email must be somebody the board can resolve. A
// sender who exists only in this binary produces mail from a person no card can
// ever be attributed to.
func TestEveryMailSendingTeamMemberIsInTheCast(t *testing.T) {
	for _, m := range demoteam.Roster {
		if !m.SendsMail() {
			continue
		}
		found := false
		for _, p := range cast {
			if p.Email == m.Email {
				found = true
				if p.Slug != m.Slug {
					t.Errorf("%s is in the cast with slug %q, want %q", m.Email, p.Slug, m.Slug)
				}
			}
		}
		if !found {
			t.Errorf("roster member %s (%s) sends mail but is not in the cast", m.Name, m.Email)
		}
	}
}

// The working agents must NOT be in the cast. They do work; they do not write
// to the mailbox, and mail from them would be the simulation talking to itself.
func TestTheWorkingAgentsDoNotSendMail(t *testing.T) {
	for _, slug := range []string{"northwind-builder", "northwind-reviewer"} {
		m, ok := demoteam.BySlug(slug)
		if !ok {
			t.Fatalf("%s is not on the roster", slug)
		}
		if m.SendsMail() {
			t.Errorf("%s has an email address; the workers do not write mail", slug)
		}
		for _, p := range cast {
			if p.Slug == slug {
				t.Errorf("%s is in the mail cast", slug)
			}
		}
	}
}

// The machine senders hold no identity: a build robot is a thing the tooling
// does, not an actor a permission rule could ever be written about.
func TestMachineSendersHoldNoIdentity(t *testing.T) {
	for _, p := range machineSenders {
		if p.Slug != "" {
			t.Errorf("machine sender %s carries identity %q", p.Email, p.Slug)
		}
		if _, ok := demoteam.ByEmail(p.Email); ok {
			t.Errorf("machine sender %s is on the roster", p.Email)
		}
	}
}

func TestEverySenderTheKindsNameExists(t *testing.T) {
	for kind, spec := range kindSpecs {
		for _, key := range spec.Senders {
			found := false
			for _, p := range cast {
				if p.Key == key {
					found = true
				}
			}
			if !found {
				t.Errorf("kind %s names sender %q, who is not in the cast", kind, key)
			}
		}
	}
}
