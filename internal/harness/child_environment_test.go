package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// writeEnvironmentRecordingHarness returns an executable that writes its
// environment to recordTo and then blocks, so the test can read what a real
// spawned harness child was handed.
func writeEnvironmentRecordingHarness(t *testing.T, recordTo string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "environment-recording-harness")
	script := "#!/bin/sh\nenv > " + recordTo + ".partial && mv " + recordTo + ".partial " + recordTo + "\nexec sleep 30\n"
	if err := os.WriteFile(binPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	return binPath
}

func waitForRecordedEnvironment(t *testing.T, recordTo string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(recordTo); err == nil {
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("harness never recorded its environment at %s", recordTo)
	return nil
}

func setEverySecretInServerEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range config.SecretEnvironmentVariableNames() {
		t.Setenv(name, "must-not-reach-a-child-"+name)
	}
}

func assertNoSecretInEnvironment(t *testing.T, environment []string) {
	t.Helper()
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		for _, secretName := range config.SecretEnvironmentVariableNames() {
			if name == secretName {
				t.Errorf("spawned child received %s", secretName)
			}
		}
	}
}

func TestAnEventsModeHarnessChildDoesNotReceiveServerSecrets(t *testing.T) {
	setEverySecretInServerEnvironment(t)
	recordTo := filepath.Join(t.TempDir(), "environment.txt")
	proc, err := StartProcess(t.Context(), writeEnvironmentRecordingHarness(t, recordTo), &store.Session{SessionID: "sess-environment"}, "cred-1", "")
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer proc.Kill()
	environment := waitForRecordedEnvironment(t, recordTo)
	assertNoSecretInEnvironment(t, environment)
	if !containsEntry(environment, "LLMBRIDGE_CREDENTIAL_ID=cred-1") {
		t.Errorf("the credential id the spawn adds is missing; the scrubbed environment replaced too much")
	}
}

func TestAPTYModeHarnessChildDoesNotReceiveServerSecrets(t *testing.T) {
	setEverySecretInServerEnvironment(t)
	recordTo := filepath.Join(t.TempDir(), "environment.txt")
	proc, err := StartProcessPTY(t.Context(), writeEnvironmentRecordingHarness(t, recordTo), &store.Session{SessionID: "sess-environment-pty"}, "", []string{"EXTRA_FROM_CALLER=1"}, "")
	if err != nil {
		t.Fatalf("StartProcessPTY: %v", err)
	}
	defer proc.Kill()
	environment := waitForRecordedEnvironment(t, recordTo)
	assertNoSecretInEnvironment(t, environment)
	if !containsEntry(environment, "LLMBRIDGE_PTY_MODE=1") || !containsEntry(environment, "EXTRA_FROM_CALLER=1") {
		t.Errorf("pty child lost the variables the spawn adds: %v", environment)
	}
}

func containsEntry(environment []string, entry string) bool {
	for _, candidate := range environment {
		if candidate == entry {
			return true
		}
	}
	return false
}
