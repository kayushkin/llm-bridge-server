package main

import (
	"strings"
	"testing"
)

func withVocabulary(t *testing.T, name string) {
	t.Helper()
	previous := active
	if err := selectVocabulary(name); err != nil {
		t.Fatalf("selectVocabulary(%q): %v", name, err)
	}
	t.Cleanup(func() { active = previous })
}

func TestAnUnknownVocabularyIsRefusedRatherThanDefaulted(t *testing.T) {
	previous := active
	defer func() { active = previous }()
	if err := selectVocabulary("wrok"); err == nil {
		t.Fatal("a misspelled vocabulary was accepted, so work mail would be scored on personal axes")
	}
	if active != previous {
		t.Fatal("a failed selection still changed the active vocabulary")
	}
}

// Every scheme has to be internally complete: an axis value with no stakes
// entry scores zero, which silently buries whatever carries it.
func TestEveryVocabularyScoresEveryAxisValue(t *testing.T) {
	for name, v := range vocabularies {
		t.Run(name, func(t *testing.T) {
			for _, action := range v.Actions {
				if _, ok := v.ActionStakes[action]; !ok {
					t.Errorf("action %q has no stakes, so it would score zero", action)
				}
			}
			for _, category := range v.Categories {
				if _, ok := v.CategoryStakes[category]; !ok {
					t.Errorf("category %q has no stakes, so it would score zero", category)
				}
			}
			for action := range v.ActionsNeedingWork {
				if !isKnown(v.Actions, action) {
					t.Errorf("action %q needs work but is not in the vocabulary", action)
				}
			}
			for action := range v.MootActionsWhenPassed {
				if !isKnown(v.Actions, action) {
					t.Errorf("moot action %q is not in the vocabulary", action)
				}
			}
			for category := range v.MootCategoriesWhenPassed {
				if !isKnown(v.Categories, category) {
					t.Errorf("moot category %q is not in the vocabulary", category)
				}
			}
			if !isKnown(v.Actions, actionNone) {
				t.Errorf("vocabulary %q has no %q action; noaction_senders.go writes it", name, actionNone)
			}
		})
	}
}

// The prompt and the tool schema must describe the SAME axes. They used to be
// able to disagree: the prompt was a package-level var evaluated at init, so it
// captured the default vocabulary whatever the flag said.
func TestThePromptDescribesTheActiveVocabulary(t *testing.T) {
	withVocabulary(t, "work")
	prompt := buildSystemPrompt()

	for _, category := range workVocabulary.Categories {
		if !strings.Contains(prompt, category) {
			t.Errorf("prompt never mentions work category %q", category)
		}
	}
	// A personal-only axis must not leak in, or the model is offered a value the
	// schema will reject.
	for _, gone := range []string{"housing", "promotions", "pay", "schedule"} {
		if strings.Contains(prompt, " "+gone+",") {
			t.Errorf("prompt still offers personal axis %q while the work vocabulary is active", gone)
		}
	}
	if !strings.Contains(prompt, workVocabulary.Domain) {
		t.Error("prompt does not say what kind of mailbox this is")
	}
}

func TestAutomationIsGatedTheWayPromotionsAre(t *testing.T) {
	withVocabulary(t, "work")
	// A build digest must not float up however loud it is: bulk by volume,
	// nothing by consequence.
	if got := stakesOf("automation", "read"); got != stakesNone {
		t.Fatalf("automation/read scored %s, want none", got)
	}
	if got := stakesOf("automation", actionNone); got != stakesNone {
		t.Fatalf("automation/none scored %s, want none", got)
	}
}

func TestSomebodyBlockedOnYouOutranksPaperwork(t *testing.T) {
	withVocabulary(t, "work")
	review := stakesOf("code-review", "review")
	admin := stakesOf("admin", "document")
	if review <= admin {
		t.Fatalf("review scored %s and admin paperwork %s; a colleague blocked on you must outrank filing", review, admin)
	}
}

func TestAnIncidentIsSevereWhateverTheVerb(t *testing.T) {
	withVocabulary(t, "work")
	if got := stakesOf("incident", "investigate"); got != stakesSevere {
		t.Fatalf("incident/investigate scored %s, want severe", got)
	}
}

// Work asked for and not done keeps costing. A review of something already
// shipped cannot be given retroactively. The two must not be treated alike.
func TestOnlyIrrecoverableWorkGoesMootOnceItsDeadlinePasses(t *testing.T) {
	withVocabulary(t, "work")
	if !isMootOncePassed("code-review", "review") {
		t.Error("a review request for shipped code should settle once passed")
	}
	if isMootOncePassed("product", "implement") {
		t.Error("implementation asked for and not done must NOT go moot; it is exactly what keeps costing")
	}
	if isMootOncePassed("incident", "investigate") {
		t.Error("an incident must never go moot on a passed deadline")
	}
	if isMootOncePassed("security", "implement") {
		t.Error("a security deadline that passed gets worse, not moot")
	}
}

func TestColumnRoutingFollowsTheActiveVocabulary(t *testing.T) {
	withVocabulary(t, "work")
	for _, action := range []string{"implement", "review", "investigate", "respond", "deploy", "decide", "escalate", "document"} {
		if got := columnForAction(action); got != columnActionNeeded {
			t.Errorf("work action %q routed to %q, want %q", action, got, columnActionNeeded)
		}
	}
	for _, action := range []string{"read", actionNone} {
		if got := columnForAction(action); got != columnNoAction {
			t.Errorf("work action %q routed to %q, want %q", action, got, columnNoAction)
		}
	}
	// An action the model invented must never be parked out of sight.
	if got := columnForAction("reticulate"); got != columnActionNeeded {
		t.Errorf("an unknown action routed to %q; it must stay visible", got)
	}
}

// The personal scheme is what the live job runs on. Nothing here may drift.
func TestThePersonalSchemeIsUnchanged(t *testing.T) {
	withVocabulary(t, "personal")
	if got := stakesOf("finance", "pay"); got != stakesSevere {
		t.Fatalf("finance/pay scored %s, want severe", got)
	}
	if got := stakesOf("housing", actionNone); got != stakesNone {
		t.Fatalf("housing/none scored %s, want none — an action of none gates the card", got)
	}
	if !isMootOncePassed("social-events", "schedule") {
		t.Error("a concert that already happened should settle")
	}
	if isMootOncePassed("finance", "pay") {
		t.Error("an overdue bill must never go moot")
	}
}
