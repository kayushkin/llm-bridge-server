package config

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// liveEnvironmentFile is a /proc/<pid>/environ to build the registry from:
//
//	go test -run TestTheLiveProcessEnvironmentBuildsARegistry ./internal/config -args -live-environment-file=/proc/<pid>/environ
//
// deploy.sh runs it against the running service before it stops it, because
// an undeclared variable under SCHEDULER_PORT, SCHEDULER_DB or SCHEDULER_TOKEN
// (SCHEDULER_DB_PATH, say) stops the new binary at boot, and with it every job.
// The job binaries' registries have their own test, in
// internal/automation/workers/shared/workersettings.
var liveEnvironmentFile = flag.String("live-environment-file", "", "a /proc/<pid>/environ to build the settings registry from")

// Only the verdict is printed, never a value: an environment holds secrets.
func TestTheLiveProcessEnvironmentBuildsARegistry(t *testing.T) {
	if *liveEnvironmentFile == "" {
		t.Skip("no -live-environment-file given")
	}
	content, err := os.ReadFile(*liveEnvironmentFile)
	if err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{}
	for _, entry := range strings.Split(string(content), "\x00") {
		if name, value, found := strings.Cut(entry, "="); found {
			variables[name] = value
		}
	}
	if len(variables) == 0 {
		t.Fatalf("%s holds no variables: that is not a process environment", *liveEnvironmentFile)
	}
	// SCHEDULER_PORT is an integer, and New quotes a value that does not parse;
	// a port is not a secret. The token is a string, which always parses, so its
	// value can never be quoted: keep a secret a string.
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatalf("the new binary would refuse to start in this environment: %v", err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Fatalf("the new binary would refuse to start in this environment: %v", err)
	}
	t.Logf("the service's registry builds from the %d variables in %s", len(variables), *liveEnvironmentFile)
}
