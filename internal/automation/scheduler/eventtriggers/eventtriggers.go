// Package eventtriggers decides which bus messages start a scheduler job.
//
// A job runs either on a cron schedule or on an event trigger: a NATS subject
// and a list of conditions on the fields of the message published there. Each
// message that meets every condition starts one run, and the job's prompt is a
// text/template filled from that message.
//
// The package knows each subject's message type, from the publisher's own Go
// type in github.com/kayushkin/bus/messages, so a trigger is checked when it is
// saved: a field the message does not have, a value of the wrong type, a regex
// that does not compile, or a prompt naming a missing field is refused then,
// rather than failing when the first message arrives.
package eventtriggers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/kayushkin/bus/messages"
)

// Trigger is the event a job waits for.
type Trigger struct {
	// Subject is the NATS subject; it must be one of Subjects.
	Subject string `json:"subject"`
	// Conditions must all hold for a message to start a run. None means every
	// message on the subject starts one.
	Conditions []Condition `json:"conditions"`
}

// Condition tests one field of the message, named by its JSON name. Exactly
// one of Equals and Regex is set.
type Condition struct {
	Field string `json:"field"`
	// Equals is a JSON value of the field's type: "discord", true, 3.
	Equals json.RawMessage `json:"equals,omitempty"`
	// Regex is an RE2 pattern the field must match; only for string fields.
	Regex string `json:"regex,omitempty"`
}

// Subject is a NATS subject a trigger may wait on, with the message type
// published there.
type Subject struct {
	Name        string
	Description string
	// MessageType is the Go type the publisher sends, decoded from JSON.
	MessageType reflect.Type
	// IdempotencyField is the JSON name of the field that identifies one
	// message. JetStream delivers at least once, so a run is recorded against
	// (job, this field's value) and a redelivered message starts nothing.
	IdempotencyField string
}

// ConsumerName is the durable JetStream consumer the scheduler reads this
// subject through. A durable name may not contain dots.
func (s Subject) ConsumerName() string {
	return "scheduler-event-triggers-" + strings.ReplaceAll(s.Name, ".", "-")
}

// Subjects lists every subject a trigger may name.
var Subjects = []Subject{
	{
		Name:             messages.MessagingInboundSubject,
		Description:      "A message a person sent on a bridged chat platform (WhatsApp, Telegram, Signal, Messenger, Discord, Slack), published by multichat.",
		MessageType:      reflect.TypeOf(messages.MessagingInbound{}),
		IdempotencyField: "event_id",
	},
}

// FindSubject returns the subject with this name.
func FindSubject(name string) (Subject, bool) {
	for _, subject := range Subjects {
		if subject.Name == name {
			return subject, true
		}
	}
	return Subject{}, false
}

// SubjectNames lists the accepted subject names, sorted.
func SubjectNames() []string {
	names := make([]string, 0, len(Subjects))
	for _, subject := range Subjects {
		names = append(names, subject.Name)
	}
	sort.Strings(names)
	return names
}

// Field is one field of a subject's message, as a trigger or a prompt names it.
type Field struct {
	Name string `json:"name"`
	// Type is "string", "boolean", "number" or "time".
	Type string `json:"type"`
	// goType and index locate the field on the Go struct.
	goType reflect.Type
	index  []int
}

var timeType = reflect.TypeOf(time.Time{})

// Fields lists the message's fields by JSON name, in declaration order.
func (s Subject) Fields() []Field {
	var fields []Field
	for i := 0; i < s.MessageType.NumField(); i++ {
		structField := s.MessageType.Field(i)
		name := strings.Split(structField.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" || !structField.IsExported() {
			continue
		}
		fields = append(fields, Field{Name: name, Type: fieldTypeName(structField.Type), goType: structField.Type, index: structField.Index})
	}
	return fields
}

func fieldTypeName(goType reflect.Type) string {
	if goType == timeType {
		return "time"
	}
	switch goType.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	default:
		return goType.String()
	}
}

func (s Subject) field(name string) (Field, bool) {
	for _, field := range s.Fields() {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}

// Validate reports why the trigger and the prompt it fills cannot run, or ""
// if they can.
func Validate(trigger Trigger, prompt string) string {
	subject, known := FindSubject(trigger.Subject)
	if !known {
		return fmt.Sprintf("event_trigger.subject must be one of %s", strings.Join(SubjectNames(), ", "))
	}
	for i, condition := range trigger.Conditions {
		if problem := validateCondition(subject, condition); problem != "" {
			return fmt.Sprintf("event_trigger.conditions[%d]: %s", i, problem)
		}
	}
	if _, err := RenderPrompt(prompt, Message{subject: subject, value: reflect.New(subject.MessageType).Elem()}); err != nil {
		return fmt.Sprintf("prompt: %s", err)
	}
	return ""
}

func validateCondition(subject Subject, condition Condition) string {
	field, known := subject.field(condition.Field)
	if !known {
		names := []string{}
		for _, f := range subject.Fields() {
			names = append(names, f.Name)
		}
		return fmt.Sprintf("field %q is not on %s messages; they have %s", condition.Field, subject.Name, strings.Join(names, ", "))
	}
	hasEquals, hasRegex := len(condition.Equals) > 0, condition.Regex != ""
	if hasEquals == hasRegex {
		return "set exactly one of equals and regex"
	}
	if hasRegex {
		if field.goType.Kind() != reflect.String {
			return fmt.Sprintf("regex works only on string fields; %s is a %s", field.Name, field.Type)
		}
		if _, err := regexp.Compile(condition.Regex); err != nil {
			return fmt.Sprintf("regex: %s", err)
		}
		return ""
	}
	if _, err := decodeEquals(field, condition.Equals); err != nil {
		return fmt.Sprintf("equals is not a %s: %s", field.Type, err)
	}
	return ""
}

func decodeEquals(field Field, equals json.RawMessage) (reflect.Value, error) {
	target := reflect.New(field.goType)
	decoder := json.NewDecoder(bytes.NewReader(equals))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return target.Elem(), nil
}

// Message is one decoded message on a subject.
type Message struct {
	subject Subject
	value   reflect.Value
}

// DecodeMessage decodes a message published on subject.
func DecodeMessage(subject Subject, data []byte) (Message, error) {
	target := reflect.New(subject.MessageType)
	if err := json.Unmarshal(data, target.Interface()); err != nil {
		return Message{}, fmt.Errorf("decode %s message: %w", subject.Name, err)
	}
	return Message{subject: subject, value: target.Elem()}, nil
}

// IdempotencyKey is the value of the subject's IdempotencyField.
func (m Message) IdempotencyKey() string {
	field, _ := m.subject.field(m.subject.IdempotencyField)
	return fmt.Sprint(m.value.FieldByIndex(field.index).Interface())
}

// fieldValues maps every field's JSON name to its value. A field the message
// left out holds its zero value, so a template naming it renders "" rather than
// failing on a key an omitempty tag dropped.
func (m Message) fieldValues() map[string]any {
	values := map[string]any{}
	for _, field := range m.subject.Fields() {
		values[field.Name] = m.value.FieldByIndex(field.index).Interface()
	}
	return values
}

// Matches reports whether the message meets every condition of the trigger.
// The trigger must have passed Validate; an error means it did not.
func Matches(trigger Trigger, message Message) (bool, error) {
	for _, condition := range trigger.Conditions {
		field, known := message.subject.field(condition.Field)
		if !known {
			return false, fmt.Errorf("field %q is not on %s messages", condition.Field, message.subject.Name)
		}
		actual := message.value.FieldByIndex(field.index)
		if condition.Regex != "" {
			pattern, err := regexp.Compile(condition.Regex)
			if err != nil {
				return false, err
			}
			if !pattern.MatchString(actual.String()) {
				return false, nil
			}
			continue
		}
		wanted, err := decodeEquals(field, condition.Equals)
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(actual.Interface(), wanted.Interface()) {
			return false, nil
		}
	}
	return true, nil
}

// RenderPrompt fills the prompt template from the message. Fields are named
// by their JSON names: {{.body}}, {{.sender_display_name}}. A name the message
// does not have is an error, not an empty string.
func RenderPrompt(prompt string, message Message) (string, error) {
	tmpl, err := template.New("prompt").Option("missingkey=error").Parse(prompt)
	if err != nil {
		return "", err
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, message.fieldValues()); err != nil {
		return "", err
	}
	return rendered.String(), nil
}
