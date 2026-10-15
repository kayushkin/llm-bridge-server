package server

// Text boxes on a run_command or send_to_session button (msg.SessionActionInput).
//
// The offer declares the boxes; the person types into them and the confirm
// carries the values (msg.SessionActionRunRequest). The values reach the
// command only as environment variables, $BRIDGE_INPUT_<name>, so the
// command's text — what the reviewer read, what Command shows, what the log
// writes — never holds them. They are kept nowhere: not in session_actions,
// not in an event, not in a log line. A secret value is also blanked wherever
// it appears in the command's output or error, in case the command prints it.
//
// On a send_to_session button the message names each box as
// $BRIDGE_INPUT_<name> (or ${BRIDGE_INPUT_<name>}), and the typed value takes
// that place in the message sent. The value is then in the target session's
// conversation, which is the point, so such a box cannot be secret; it is
// still kept nowhere in session_actions, its events or this server's log.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/kayushkin/llm-bridge/msg"
)

const (
	maximumSessionActionInputs          = 5
	maximumSessionActionInputValueBytes = 4096
	maximumSessionActionRunBodyBytes    = 64 << 10
	// hiddenSessionActionInput replaces a secret value found in output.
	hiddenSessionActionInput = "[hidden input]"
	// minimumBlankedSecretCharacters keeps a one- or two-character value from
	// blanking every match of it in an output; a secret that short is no secret.
	minimumBlankedSecretCharacters = 3
)

var sessionActionInputNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

// sessionActionInputsProblem says what is wrong with an offer's inputs, or "".
func sessionActionInputsProblem(inputs []msg.SessionActionInput) string {
	if len(inputs) > maximumSessionActionInputs {
		return fmt.Sprintf("a button has at most %d inputs, not %d", maximumSessionActionInputs, len(inputs))
	}
	seen := map[string]bool{}
	for _, input := range inputs {
		switch {
		case !sessionActionInputNamePattern.MatchString(input.Name):
			return fmt.Sprintf("input name %q must be upper case letters, digits and underscores, start with a letter, and be at most 32 characters", input.Name)
		case seen[input.Name]:
			return fmt.Sprintf("input %s is declared twice", input.Name)
		case strings.TrimSpace(input.Label) == "":
			return fmt.Sprintf("input %s needs a label: the text beside its box", input.Name)
		case len([]rune(input.Label)) > maximumSessionActionLabelCharacters:
			return fmt.Sprintf("input %s's label is longer than %d characters", input.Name, maximumSessionActionLabelCharacters)
		case strings.IndexFunc(input.Label, unicode.IsControl) >= 0:
			return fmt.Sprintf("input %s's label cannot contain a control character", input.Name)
		}
		seen[input.Name] = true
	}
	return ""
}

// describeSessionActionInputs is the part of Command that names the boxes and
// the variables their values arrive in.
func describeSessionActionInputs(inputs []msg.SessionActionInput) string {
	if len(inputs) == 0 {
		return ""
	}
	parts := make([]string, len(inputs))
	for index, input := range inputs {
		kind := "text"
		if input.Secret {
			kind = "secret"
		}
		parts[index] = fmt.Sprintf("$%s%s (%s, %q)", msg.SessionActionInputEnvironmentPrefix, input.Name, kind, input.Label)
	}
	return "\nwith what the person types into its boxes in " + strings.Join(parts, ", ")
}

// readSessionActionRunInputs reads a confirm's body and holds it to the
// offer's inputs: a non-empty value for each, none for anything else. An
// empty body is a confirm with no values, which a button with no inputs needs.
func readSessionActionRunInputs(r *http.Request, w http.ResponseWriter, inputs []msg.SessionActionInput) (map[string]string, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maximumSessionActionRunBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("the confirm's body could not be read: %w", err)
	}
	var request msg.SessionActionRunRequest
	if strings.TrimSpace(string(body)) != "" {
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			return nil, fmt.Errorf("the confirm's body is not {\"inputs\": {name: value}}: %w", err)
		}
	}
	declared := map[string]bool{}
	for _, input := range inputs {
		declared[input.Name] = true
		value, given := request.Inputs[input.Name]
		switch {
		case !given || value == "":
			return nil, fmt.Errorf("input %s (%q) needs a value", input.Name, input.Label)
		case len(value) > maximumSessionActionInputValueBytes:
			return nil, fmt.Errorf("input %s is longer than %d bytes", input.Name, maximumSessionActionInputValueBytes)
		case strings.ContainsRune(value, 0):
			return nil, fmt.Errorf("input %s contains a NUL byte, which no environment variable can hold", input.Name)
		}
	}
	for name := range request.Inputs {
		if !declared[name] {
			return nil, fmt.Errorf("this button has no input %q", name)
		}
	}
	return request.Inputs, nil
}

// sessionActionInputEnvironment is the inputs as environment assignments.
func sessionActionInputEnvironment(values map[string]string) []string {
	environment := make([]string, 0, len(values))
	for name, value := range values {
		environment = append(environment, msg.SessionActionInputEnvironmentPrefix+name+"="+value)
	}
	return environment
}

// blankSecretSessionActionInputs replaces every secret value in an outcome's
// output and error with a marker, so a command that printed one does not put
// it in the record, the event or the chat.
func blankSecretSessionActionInputs(outcome sessionActionOutcome, inputs []msg.SessionActionInput, values map[string]string) sessionActionOutcome {
	for _, input := range inputs {
		value := values[input.Name]
		if !input.Secret || len([]rune(value)) < minimumBlankedSecretCharacters {
			continue
		}
		outcome.output = strings.ReplaceAll(outcome.output, value, hiddenSessionActionInput)
		if outcome.err != nil && strings.Contains(outcome.err.Error(), value) {
			outcome.err = errors.New(strings.ReplaceAll(outcome.err.Error(), value, hiddenSessionActionInput))
		}
	}
	return outcome
}

// sessionActionMessageInputReferencePattern finds $BRIDGE_INPUT_NAME and
// ${BRIDGE_INPUT_NAME} in a send_to_session message.
var sessionActionMessageInputReferencePattern = regexp.MustCompile(
	`\$\{` + msg.SessionActionInputEnvironmentPrefix + `([A-Z][A-Z0-9_]*)\}|\$` + msg.SessionActionInputEnvironmentPrefix + `([A-Z][A-Z0-9_]*)`)

func sessionActionMessageInputReferenceName(submatches []string) string {
	if submatches[1] != "" {
		return submatches[1]
	}
	return submatches[2]
}

// sessionActionMessageInputsProblem says what is wrong with a send_to_session
// message and its boxes, or "": no box may be secret, every box must be named
// in the message, and the message may name no box the button does not have.
func sessionActionMessageInputsProblem(message string, inputs []msg.SessionActionInput) string {
	declared := map[string]bool{}
	for _, input := range inputs {
		if input.Secret {
			return fmt.Sprintf("input %s cannot be secret: a send_to_session value is written into the target session's conversation", input.Name)
		}
		declared[input.Name] = true
	}
	referenced := map[string]bool{}
	for _, submatches := range sessionActionMessageInputReferencePattern.FindAllStringSubmatch(message, -1) {
		name := sessionActionMessageInputReferenceName(submatches)
		if !declared[name] {
			return fmt.Sprintf("the message names $%s%s, but the button has no input %s", msg.SessionActionInputEnvironmentPrefix, name, name)
		}
		referenced[name] = true
	}
	for _, input := range inputs {
		if !referenced[input.Name] {
			return fmt.Sprintf("input %s is never used: name it in the message as $%s%s, where the typed value goes", input.Name, msg.SessionActionInputEnvironmentPrefix, input.Name)
		}
	}
	return ""
}

// fillSessionActionMessageInputs puts each typed value in the place the
// message names it. The offer was checked to name only its own boxes, and the
// confirm to fill every one.
func fillSessionActionMessageInputs(message string, values map[string]string) string {
	return sessionActionMessageInputReferencePattern.ReplaceAllStringFunc(message, func(reference string) string {
		return values[sessionActionMessageInputReferenceName(sessionActionMessageInputReferencePattern.FindStringSubmatch(reference))]
	})
}

// describeSessionActionMessageInputs is the part of a send_to_session Command
// that names the boxes and where their values go.
func describeSessionActionMessageInputs(inputs []msg.SessionActionInput) string {
	if len(inputs) == 0 {
		return ""
	}
	parts := make([]string, len(inputs))
	for index, input := range inputs {
		parts[index] = fmt.Sprintf("what the person types into %q replaces $%s%s", input.Label, msg.SessionActionInputEnvironmentPrefix, input.Name)
	}
	return "\n(" + strings.Join(parts, "; ") + ")"
}
