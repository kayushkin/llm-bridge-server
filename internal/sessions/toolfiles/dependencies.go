package toolfiles

import (
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	Harness       *harness.Manager
	SnapshotStore func() *snapshots.Store
	Store         *store.Store
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
