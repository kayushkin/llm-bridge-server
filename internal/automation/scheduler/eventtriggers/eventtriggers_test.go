package eventtriggers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kayushkin/bus/messages"
)

func inboundSubject(t *testing.T) Subject {
	t.Helper()
	subject, known := FindSubject(messages.MessagingInboundSubject)
	if !known {
		t.Fatal("messaging.inbound is not a known subject")
	}
	return subject
}

func inboundMessage(t *testing.T, inbound messages.MessagingInbound) Message {
	t.Helper()
	data, err := json.Marshal(inbound)
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeMessage(inboundSubject(t), data)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestValidateRefusesWhatCouldNotRun(t *testing.T) {
	cases := []struct {
		name    string
		trigger Trigger
		prompt  string
		refusal string
	}{
		{"unknown subject", Trigger{Subject: "messaging.outbound"}, "hi", "subject must be one of messaging.inbound"},
		{"unknown field", Trigger{Subject: "messaging.inbound", Conditions: []Condition{{Field: "sender", Equals: json.RawMessage(`"x"`)}}}, "hi", `field "sender" is not on messaging.inbound messages`},
		{"neither test", Trigger{Subject: "messaging.inbound", Conditions: []Condition{{Field: "body"}}}, "hi", "set exactly one of equals and regex"},
		{"both tests", Trigger{Subject: "messaging.inbound", Conditions: []Condition{{Field: "body", Equals: json.RawMessage(`"x"`), Regex: "x"}}}, "hi", "set exactly one of equals and regex"},
		{"wrong type", Trigger{Subject: "messaging.inbound", Conditions: []Condition{{Field: "is_direct_message", Equals: json.RawMessage(`"yes"`)}}}, "hi", "equals is not a boolean"},
		{"regex on a boolean", Trigger{Subject: "messaging.inbound", Conditions: []Condition{{Field: "is_direct_message", Regex: "t"}}}, "hi", "regex works only on string fields"},
		{"bad regex", Trigger{Subject: "messaging.inbound", Conditions: []Condition{{Field: "body", Regex: "("}}}, "hi", "regex:"},
		{"prompt names a missing field", Trigger{Subject: "messaging.inbound"}, "{{.sender}} wrote", `map has no entry for key "sender"`},
		{"prompt does not parse", Trigger{Subject: "messaging.inbound"}, "{{.body", "prompt:"},
	}
	for _, c := range cases {
		got := Validate(c.trigger, c.prompt)
		if !strings.Contains(got, c.refusal) {
			t.Errorf("%s: Validate = %q, want it to contain %q", c.name, got, c.refusal)
		}
	}
}

func TestValidateAcceptsATriggerThatCanRun(t *testing.T) {
	trigger := Trigger{Subject: "messaging.inbound", Conditions: []Condition{
		{Field: "platform", Equals: json.RawMessage(`"discord"`)},
		{Field: "is_direct_message", Equals: json.RawMessage(`true`)},
		{Field: "body", Regex: `(?i)^@claude`},
	}}
	if problem := Validate(trigger, "{{.sender_display_name}} wrote on {{.platform}}: {{.body}}"); problem != "" {
		t.Fatalf("Validate = %q", problem)
	}
}

func TestMatchesNeedsEveryCondition(t *testing.T) {
	trigger := Trigger{Subject: "messaging.inbound", Conditions: []Condition{
		{Field: "platform", Equals: json.RawMessage(`"discord"`)},
		{Field: "is_direct_message", Equals: json.RawMessage(`true`)},
		{Field: "body", Regex: `(?i)^@claude`},
	}}
	cases := []struct {
		name    string
		inbound messages.MessagingInbound
		want    bool
	}{
		{"all hold", messages.MessagingInbound{Platform: "discord", IsDirectMessage: true, Body: "@Claude hello"}, true},
		{"other platform", messages.MessagingInbound{Platform: "telegram", IsDirectMessage: true, Body: "@claude hello"}, false},
		{"group room", messages.MessagingInbound{Platform: "discord", IsDirectMessage: false, Body: "@claude hello"}, false},
		{"no mention", messages.MessagingInbound{Platform: "discord", IsDirectMessage: true, Body: "hello"}, false},
	}
	for _, c := range cases {
		got, err := Matches(trigger, inboundMessage(t, c.inbound))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNoConditionsMatchesEveryMessage(t *testing.T) {
	got, err := Matches(Trigger{Subject: "messaging.inbound"}, inboundMessage(t, messages.MessagingInbound{Body: "anything"}))
	if err != nil || !got {
		t.Fatalf("Matches = %v, %v", got, err)
	}
}

// room_name and sender_display_name are omitempty, so a direct message arrives
// without them. The prompt still renders; the field is empty.
func TestRenderPromptFillsFieldsAMessageLeftOut(t *testing.T) {
	message := inboundMessage(t, messages.MessagingInbound{EventID: "$event", Platform: "signal", Body: "are you free?"})
	got, err := RenderPrompt("[{{.room_name}}] {{.platform}}: {{.body}}", message)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[] signal: are you free?" {
		t.Fatalf("RenderPrompt = %q", got)
	}
	if key := message.IdempotencyKey(); key != "$event" {
		t.Fatalf("IdempotencyKey = %q", key)
	}
}

func TestConsumerNameHasNoDots(t *testing.T) {
	if got := inboundSubject(t).ConsumerName(); got != "scheduler-event-triggers-messaging-inbound" {
		t.Fatalf("ConsumerName = %q", got)
	}
}
