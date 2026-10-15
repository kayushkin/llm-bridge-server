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
	if sessionActionReportsToItsSession(msg.SessionActionDeploy) {
		t.Error("deploy-gate already reports a deploy; the server must not report it again")
	}
	for _, kind := range []msg.SessionActionType{msg.SessionActionRunCommand, msg.SessionActionModelCall, msg.SessionActionBackgroundAgent,
		msg.SessionActionRunSchedulerJob, msg.SessionActionForkAndSend, msg.SessionActionNewSessionAndSend} {
		if !sessionActionReportsToItsSession(kind) {
			t.Errorf("%s is not reported", kind)
		}
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
