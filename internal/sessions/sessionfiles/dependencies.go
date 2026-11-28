package sessionfiles

import (
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	Cfg            *config.Config
	FileStore      func() SessionFileStore
	Harness        *harness.Manager
	MessageSenders *messagesender.Senders
	Store          *store.Store
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
