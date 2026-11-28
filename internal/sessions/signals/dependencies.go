package signals

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/parkedasks"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"
	"github.com/kayushkin/llm-bridge/msg"
)

// Dependencies is what the signals read from the server that composes them.
type Dependencies struct {
	Harness          *harness.Manager
	HarnessStore     func() *machines.Store
	IssueReads       func() *issuereads.Reader
	MailMessages     func() MailMessageReader
	MessageSenders   *messagesender.Senders
	ParkedAsks       *parkedasks.Asks
	QuestionTriage   func() *QuestionTriage
	SignalClassifier func() *SignalClassifier
	Store            *store.Store
	Sessions         Host
}

// Host holds a message while the server drains for a restart, and starts a
// session's harness, as answering a signal does.
type Host interface {
	HoldMessageIfRestartDraining(sessionID string, request msg.SendMessageRequest, senderPrincipalID string, source string) (heldMessageID string, held bool, err error)
	StartOnInstance(ctx context.Context, sess *store.Session, inst *msg.Instance, credID string) (harness.HarnessProcess, error)
}

// Service raises, lists, answers and resolves a session's signals: its
// questions, its decisions and what the turn-end classifier finds.
type Service struct {
	Dependencies
}
