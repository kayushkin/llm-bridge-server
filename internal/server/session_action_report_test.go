package server

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestTheReportTellsTheAgentHowItsButtonEnded(t *testing.T) {
	report := sessionActionReport(store.SessionAction{
		ActionID: "session_action_000039",
		Offer:    msg.SessionActionOffer{Label: "Save the Mac password", Type: msg.SessionActionRunCommand},
		State:    msg.SessionActionSucceeded,
		Output:   "saved as auth-store credential cred_1\n",
	})
	for _, want := range []string{"session_action_000039", `"Save the Mac password"`, "succeeded", "Output:\nsaved as auth-store credential cred_1", "not a new instruction"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}

	failed := sessionActionReport(store.SessionAction{
		ActionID: "session_action_000040", Offer: msg.SessionActionOffer{Label: "Research", Type: msg.SessionActionBackgroundAgent},
		State: msg.SessionActionFailed, Error: "it ran longer than 1h", ResultSessionID: "br_child",
	})
	for _, want := range []string{"failed", "Error: it ran longer than 1h", "session br_child"} {
		if !strings.Contains(failed, want) {
			t.Errorf("report lacks %q:\n%s", want, failed)
		}
	}
	if strings.Contains(failed, "Output:") {
		t.Error("a report with no output has an Output section")
	}
}

func TestADeployIsNotReportedTwice(t *testing.T) {
	for _, state := range []msg.SessionActionState{msg.SessionActionSucceeded, msg.SessionActionFailed} {
		deploy := store.SessionAction{Offer: msg.SessionActionOffer{Type: msg.SessionActionDeploy}, State: state}
		if sessionActionReportsToItsSession(deploy) {
			t.Errorf("a %s deploy is reported; deploy-gate already reports it", state)
		}
	}
}

func TestAPressIsReportedWhenItFailsOrTheOfferAskedForItsSuccess(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name          string
		reportSuccess *bool
		state         msg.SessionActionState
		want          bool
	}{
		{"success the offer asked for", &yes, msg.SessionActionSucceeded, true},
		{"success the offer did not ask for", &no, msg.SessionActionSucceeded, false},
		{"failure the offer did not ask for", &no, msg.SessionActionFailed, true},
		{"unseen end the offer did not ask for", &no, msg.SessionActionOutcomeUnknown, true},
		{"success of an action offered before the choice existed", nil, msg.SessionActionSucceeded, true},
	} {
		action := store.SessionAction{Offer: msg.SessionActionOffer{Type: msg.SessionActionModelCall, ReportSuccess: c.reportSuccess}, State: c.state}
		if got := sessionActionReportsToItsSession(action); got != c.want {
			t.Errorf("%s: reported %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTheReportIsMarkedAsABridgeReport(t *testing.T) {
	report := sessionActionReport(store.SessionAction{ActionID: "session_action_000042", Offer: msg.SessionActionOffer{Label: "x"}, State: msg.SessionActionFailed})
	if !strings.HasPrefix(report, `<bridge-report kind="session-action">`) || !strings.HasSuffix(report, "</bridge-report>") {
		t.Errorf("the report is not one bridge-report block:\n%s", report)
	}
}

func TestASecretTypedIntoAButtonNeverReachesTheReport(t *testing.T) {
	const secret = "correct horse battery staple"
	inputs := []msg.SessionActionInput{{Name: "PASSWORD", Label: "p", Secret: true}}
	outcome := blankSecretSessionActionInputs(sessionActionOutcome{output: "echoed " + secret}, inputs, map[string]string{"PASSWORD": secret})
	report := sessionActionReport(store.SessionAction{ActionID: "session_action_000041", Offer: msg.SessionActionOffer{Label: "x", Inputs: inputs},
		State: msg.SessionActionSucceeded, Output: outcome.output})
	if strings.Contains(report, secret) {
		t.Errorf("the report carries the secret:\n%s", report)
	}
}
