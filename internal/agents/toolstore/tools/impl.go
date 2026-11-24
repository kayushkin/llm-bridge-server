// Package tools holds local tool implementations that ship inside the
// tool-store binary. Each tool registers itself via Register() and is exposed
// by tool-store as a kind=local entry, invokable via POST /tools/{name}/invoke.
package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/kayushkin/tool-store/schema"
)

// Impl is one local tool: name, human description, JSON Schema for inputs, and
// a Run function that takes the JSON-encoded input string and returns a string
// result (or an error). Mirrors the agentkit.Tool shape but with a SDK-free
// schema type.
type Impl struct {
	Name        string
	Description string
	InputSchema schema.InputSchema
	Run         func(ctx context.Context, input string) (string, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Impl{}
)

// Register adds a tool to the in-process registry. Calling twice with the same
// name overwrites the prior registration (intentional — lets a binary swap a
// stock implementation for a customized one). Panics if the name is empty or
// Run is nil — those are programmer errors that should fail at startup.
func Register(t Impl) {
	if t.Name == "" {
		panic("tools.Register: empty Name")
	}
	if t.Run == nil {
		panic(fmt.Sprintf("tools.Register: nil Run for %q", t.Name))
	}
	registryMu.Lock()
	registry[t.Name] = t
	registryMu.Unlock()
}

// ByName returns the registered tool with the given name, or false if none.
func ByName(name string) (Impl, bool) {
	registryMu.RLock()
	t, ok := registry[name]
	registryMu.RUnlock()
	return t, ok
}

// All returns every registered tool, sorted by name for stable iteration.
func All() []Impl {
	registryMu.RLock()
	out := make([]Impl, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	registryMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
