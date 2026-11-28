package inventoryroutes

import (
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/serviceinventory"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	Cfg *config.Config
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
	ServiceInventory   *serviceinventory.Inventory
	ServiceInventoryMu sync.Mutex
}
