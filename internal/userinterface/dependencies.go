package userinterface

import (
	"io/fs"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	UserInterfaceBuild func() fs.FS
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
