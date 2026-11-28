package briefs

import (
	"context"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionanswer"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Dependencies is what the brief pass and the brief routes read from the
// server that composes them.
type Dependencies struct {
	Harness *harness.Manager
	// TurnClassifier is the turn-end signal classifier, nil while it is off.
	TurnClassifier func() TurnClassifier
	Store          *store.Store
	Sessions       SessionMessenger
}

// TurnClassifier is the turn-end signal classifier as the brief pass reads
// it: the brief rides on its switch, model role, timeout and one-shot runner.
type TurnClassifier interface {
	EnabledFor(harness msg.Harness) bool
	CurrentModelRole() string
	CurrentTimeout() time.Duration
	RunOneShot(ctx context.Context, request msg.OneShotRequest) ([]byte, error)
}

// SessionMessenger sends into a session as this server does.
type SessionMessenger interface {
	SendMessageFromThisServer(ctx context.Context, sessionID string, req msg.SendMessageRequest, senderPrincipalID string) sessionanswer.Answer
}

// Service runs the brief pass and answers the brief routes.
type Service struct {
	Dependencies
}
