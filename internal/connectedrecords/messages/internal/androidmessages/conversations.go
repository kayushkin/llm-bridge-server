package androidmessages

import "strings"

// MaximumMembers is the most people, the user included, a conversation may
// have to be imported: direct conversations and small groups.
const MaximumMembers = 8

// Conversation is everyone but the user (Others) and what was said.
type Conversation struct {
	Others   []string
	Messages []*Message
	// Calls are the call-log rows with the one other person of a direct
	// conversation.
	Calls []*Call
	// CallsOnly is a direct conversation made for a number the user called
	// or was called by but kept no texts with.
	CallsOnly bool
}

// Key is the conversation's numbers joined by commas.
func (conversation *Conversation) Key() string { return strings.Join(conversation.Others, ",") }

// Skipped counts what Conversations left out, conversations and their
// messages.
type Skipped struct {
	// NotPhoneNumber: someone in it is a short code or an email address.
	NotPhoneNumber, NotPhoneNumberMessages int
	// TooBig: more than MaximumMembers people.
	TooBig, TooBigMessages int
	// UserNeverWrote: the user sent nothing in it (alerts, codes, mass texts).
	UserNeverWrote, UserNeverWroteMessages int
	// NoOneElse: a message with no other address at all.
	NoOneElseMessages int
	// CallsNotPhoneNumber: calls whose number is not a phone number (a
	// private or unknown caller); BlockedCalls: calls the phone blocked.
	CallsNotPhoneNumber, BlockedCalls int
}

// Conversations groups the archive's messages by who is in them and keeps
// the conversations the user took part in: everyone a phone number, at most
// MaximumMembers people, and at least one message from the user. Calls join
// the direct conversation with their number; a phone number with no kept
// texts gets a direct conversation of its calls alone, after the others.
func Conversations(archive *Archive) ([]*Conversation, Skipped) {
	var skipped Skipped
	byKey := map[string]*Conversation{}
	var order []string
	for _, message := range archive.Messages {
		if len(message.Others) == 0 {
			skipped.NoOneElseMessages++
			continue
		}
		key := strings.Join(message.Others, ",")
		conversation := byKey[key]
		if conversation == nil {
			conversation = &Conversation{Others: message.Others}
			byKey[key] = conversation
			order = append(order, key)
		}
		conversation.Messages = append(conversation.Messages, message)
	}
	var kept []*Conversation
	keptByKey := map[string]*Conversation{}
	for _, key := range order {
		conversation := byKey[key]
		count := len(conversation.Messages)
		notPhone := false
		for _, other := range conversation.Others {
			if E164(other) != other {
				notPhone = true
			}
		}
		userWrote := false
		for _, message := range conversation.Messages {
			userWrote = userWrote || message.FromSelf
		}
		switch {
		case notPhone:
			skipped.NotPhoneNumber++
			skipped.NotPhoneNumberMessages += count
		case len(conversation.Others)+1 > MaximumMembers:
			skipped.TooBig++
			skipped.TooBigMessages += count
		case !userWrote:
			skipped.UserNeverWrote++
			skipped.UserNeverWroteMessages += count
		default:
			kept = append(kept, conversation)
			keptByKey[key] = conversation
		}
	}
	for _, call := range archive.Calls {
		if call.Type == CallBlocked {
			skipped.BlockedCalls++
			continue
		}
		conversation := keptByKey[call.Number]
		if conversation == nil {
			if call.Number == "" || E164(call.Number) != call.Number {
				skipped.CallsNotPhoneNumber++
				continue
			}
			conversation = &Conversation{Others: []string{call.Number}, CallsOnly: true}
			keptByKey[call.Number] = conversation
			kept = append(kept, conversation)
		}
		conversation.Calls = append(conversation.Calls, call)
	}
	return kept, skipped
}
