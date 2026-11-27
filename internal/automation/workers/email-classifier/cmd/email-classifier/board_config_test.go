package main

import (
	"sort"
	"strings"
	"testing"
)

// The board's classifier setting names exactly the mailstack accounts a tick
// reads, in the board's order. Until 2026-09-11 an empty -account meant every
// enabled account, and the personal Email board's job filed 231 messages from
// the synthetic demo-work account onto 30 of its cards.
func TestMailAccountsAreExactlyTheOnesTheBoardNames(t *testing.T) {
	listed := []mailAccount{
		{ID: "gmail-personal", Enabled: true},
		{ID: "imap-pretender", Enabled: true},
		{ID: "demo-work", Enabled: true},
	}
	got, err := selectMailAccounts(listed, []string{"imap-pretender", "gmail-personal"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	var ids []string
	for _, account := range got {
		ids = append(ids, account.ID)
	}
	if strings.Join(ids, ",") != "imap-pretender,gmail-personal" {
		t.Fatalf("selected %v, want exactly the board's two in the board's order — demo-work is listed by mailstack but not named by the board", ids)
	}
}

func TestABoardNamingNoAccountsIsRefusedNotReadAsEveryAccount(t *testing.T) {
	listed := []mailAccount{{ID: "gmail-personal", Enabled: true}}
	if _, err := selectMailAccounts(listed, nil); err == nil {
		t.Fatal("an empty account list was accepted; it must never mean every account")
	}
}

func TestAnAccountMailstackDoesNotListIsNamed(t *testing.T) {
	listed := []mailAccount{{ID: "gmail-personal", Enabled: true}, {ID: "demo-work", Enabled: true}}
	_, err := selectMailAccounts(listed, []string{"gmail-personal", "gone-account"})
	if err == nil {
		t.Fatal("an account mailstack does not list was accepted; the tick would quietly file from the rest")
	}
	for _, want := range []string{"gone-account", "gmail-personal", "demo-work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q; it must name the missing id and what mailstack does list", err, want)
		}
	}
}

// vocabularyNames is the one list behind -list-vocabularies and behind the
// refusal of an unknown name, so a name the settings page offers is always one
// selectVocabulary accepts.
func TestEveryListedVocabularyIsSelectableAndNothingElseIs(t *testing.T) {
	t.Cleanup(func() { active = &personalVocabulary })
	names := vocabularyNames()
	if !sort.StringsAreSorted(names) {
		t.Errorf("vocabularyNames() = %v, not sorted", names)
	}
	for _, want := range []string{"personal", "work"} {
		if !contains(names, want) {
			t.Errorf("vocabularyNames() = %v, missing %q", names, want)
		}
	}
	for _, name := range names {
		if err := selectVocabulary(name); err != nil {
			t.Errorf("listed vocabulary %q is refused by selectVocabulary: %v", name, err)
		}
	}
	err := selectVocabulary("wrok")
	if err == nil {
		t.Fatal("an unknown vocabulary was selected; a typo would classify against the wrong axes")
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not name the known vocabulary %q", err, name)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
