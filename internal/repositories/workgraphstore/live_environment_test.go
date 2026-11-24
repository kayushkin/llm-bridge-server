package workgraphstore

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// liveEnvironmentFile is a /proc/<pid>/environ to build the registry from:
//
//	go test -run TestTheLiveProcessEnvironmentBuildsARegistry . -args -live-environment-file=/proc/<pid>/environ
//
// deploy.sh runs it against the running service before it stops it, because a
// set WORK_GRAPH_STORE_ variable that nobody declared stops the new binary at boot.
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
	// A setting that does not parse has its bad value quoted in New's error.
	// None of these settings is a secret, so that is safe to print.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables)); err != nil {
		t.Fatalf("the new binary would refuse to start in this environment: %v", err)
	}
	t.Logf("a registry builds from the %d variables in %s", len(variables), *liveEnvironmentFile)
}
