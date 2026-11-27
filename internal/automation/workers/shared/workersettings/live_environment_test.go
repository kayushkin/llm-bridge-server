package workersettings

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// liveEnvironmentFile is a /proc/<pid>/environ to build every job binary's
// registry from:
//
//	go test -run TestTheLiveProcessEnvironmentBuildsEveryJobBinarysRegistry ./internal/automation/workers/shared/workersettings -args -live-environment-file=/proc/<pid>/environ
//
// Point it at the process that runs the jobs: every job starts with that
// environment, and a job binary refuses a misspelling of a name it owns.
var liveEnvironmentFile = flag.String("live-environment-file", "", "a /proc/<pid>/environ to build every job binary's registry from")

// Only the verdict is printed, never a value: an environment holds secrets.
func TestTheLiveProcessEnvironmentBuildsEveryJobBinarysRegistry(t *testing.T) {
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
	for _, binary := range JobBinaries {
		registry, err := binary.NewSettingsRegistry(servicesettings.MapEnvironment(variables))
		if err != nil {
			t.Errorf("%s would refuse to start in this environment: %v", binary.Name, err)
			continue
		}
		if err := registry.CheckRequired(); err != nil {
			t.Errorf("%s would refuse to start in this environment: %v", binary.Name, err)
		}
	}
	t.Logf("%d job binaries' registries build from the %d variables in %s", len(JobBinaries), len(variables), *liveEnvironmentFile)
}
