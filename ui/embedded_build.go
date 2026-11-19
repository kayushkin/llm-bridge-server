// Package ui holds the web front end: the bridge-ui and chat-core packages, and
// the build of them that llm-bridge-server serves itself at /ui/.
package ui

import (
	"embed"
	"io/fs"
)

// builtFiles is whatever scripts/build-ui.sh left in built/app when this binary
// was compiled. built/ is tracked with one placeholder so a clean clone still
// compiles; the build output is never committed.
//
//go:embed all:built
var builtFiles embed.FS

// Build returns the built front end, rooted at its index.html. A binary
// compiled before scripts/build-ui.sh ran holds no index.html, and whoever
// serves the build must say so rather than serve nothing.
func Build() fs.FS {
	build, err := fs.Sub(builtFiles, "built/app")
	if err != nil {
		panic("ui: built/app is not a valid path inside the embedded files: " + err.Error())
	}
	return build
}
