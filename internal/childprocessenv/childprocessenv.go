// Package childprocessenv builds the environment of every process this server
// spawns: harness wrappers (events, pty, ssh), the OTel sidecar, oneshot calls,
// discovery and history import, registered hook commands, git, and the
// conformance runner.
//
// Those processes run agents, and an agent with a shell can read its own
// environment. Before this package every spawn path handed the child
// os.Environ() — including LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY, with which an
// agent could mint a login cookie for any principal. Every spawn now takes its
// environment from EnvironmentWithoutServerSecrets, which removes the names
// config.SecretEnvironmentVariableNames declares.
//
// An exec.Cmd whose Env is left nil inherits the full parent environment, so a
// spawn path that forgets to set Env leaks exactly as before; every exec in
// this module sets it from here.
package childprocessenv

import (
	"os"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// EnvironmentWithoutServerSecrets returns this process's environment with
// every variable named by config.SecretEnvironmentVariableNames removed. The
// result is a fresh slice the caller may append to.
func EnvironmentWithoutServerSecrets() []string {
	return RemoveServerSecrets(os.Environ())
}

// RemoveServerSecrets returns environment ("NAME=value" entries) without any
// entry whose name is a server secret. Every occurrence is removed, so a
// duplicated entry cannot survive. Names are compared exactly, as the kernel
// and the Go runtime compare them on Unix.
func RemoveServerSecrets(environment []string) []string {
	secretNames := config.SecretEnvironmentVariableNames()
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		isSecret := false
		for _, secretName := range secretNames {
			if name == secretName {
				isSecret = true
				break
			}
		}
		if !isSecret {
			kept = append(kept, entry)
		}
	}
	return kept
}
