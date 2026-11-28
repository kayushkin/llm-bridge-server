package permissiongate

import (
	"github.com/kayushkin/llm-bridge-server/internal/agents/hooks"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/parkedasks"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	BridgePrefs      BridgePrefs
	Harness          *harness.Manager
	HookStore        *hooks.Store
	ParkedAsks       *parkedasks.Asks
	PermissionEngine func() PermissionEngine
	Signals          *signals.Service
	Store            *store.Store
	Sessions         Host
}

// Host is what this package calls on the server that composes it.
type Host interface {
	MaybeCaptureSnapshot(event string, sessionID string, body []byte)
}

// BridgePrefs is what this package calls on the server's BridgePrefs.
type BridgePrefs interface {
	Get() msg.BridgePrefs
	SetPermissionMode(mode string)
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
