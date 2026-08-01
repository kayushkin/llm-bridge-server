package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestWorkingDirForInstanceAppliesTheInstanceOverMachineCascade(t *testing.T) {
	machine := &msg.Machine{ID: "m1", DefaultWorkingDir: "/srv/machine-default"}

	cases := []struct {
		name string
		inst *msg.Instance
		want string
	}{
		{
			name: "instance override wins over the machine default",
			inst: &msg.Instance{ID: "i1", WorkingDir: "/srv/instance", Machine: machine},
			want: "/srv/instance",
		},
		{
			name: "an empty instance dir inherits the machine default",
			inst: &msg.Instance{ID: "i1", Machine: machine},
			want: "/srv/machine-default",
		},
		{
			name: "neither configured resolves to empty, meaning inherit",
			inst: &msg.Instance{ID: "i1", Machine: &msg.Machine{ID: "m2"}},
			want: "",
		},
		{
			name: "an instance dir still applies when the machine is absent",
			inst: &msg.Instance{ID: "i1", WorkingDir: "/srv/instance"},
			want: "/srv/instance",
		},
		{
			name: "no machine and no instance dir is empty, not a panic",
			inst: &msg.Instance{ID: "i1"},
			want: "",
		},
		{
			name: "a nil instance is empty, not a panic",
			inst: nil,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workingDirForInstance(tc.inst); got != tc.want {
				t.Errorf("workingDirForInstance() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestALocalHarnessRunsInTheInstanceWorkingDirectory is the proof that the
// resolved directory reaches the operating system, not merely a variable.
//
// It spawns the real StartProcess against a stand-in harness that records the
// directory it was started in. Against the previous code — which never set
// cmd.Dir — the child inherits the test binary's own directory and this fails.
func TestALocalHarnessRunsInTheInstanceWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	binPath := writeCwdRecordingHarness(t, recordTo)

	proc, err := StartProcess(t.Context(), binPath, &store.Session{SessionID: "sess-local"}, "", workingDir)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer proc.Kill()

	got := waitForRecordedCwd(t, recordTo)

	// t.TempDir() can sit under a symlinked root (/tmp → /private/tmp and
	// similar), and the child reports the resolved path. Compare resolved
	// against resolved so the assertion is about the directory, not its spelling.
	if want := resolvePath(t, workingDir); got != want {
		t.Errorf("harness ran in %q, want the instance working directory %q", got, want)
	}
	if got == resolvePath(t, mustGetwd(t)) {
		t.Errorf("harness inherited bridge-server's own directory %q — cmd.Dir was not applied", got)
	}
}

// TestALocalHarnessWithNoWorkingDirectoryInheritsTheServersOwn pins the other
// half of the contract: an unconfigured instance must keep behaving exactly as
// it did before, rather than being sent somewhere invented.
func TestALocalHarnessWithNoWorkingDirectoryInheritsTheServersOwn(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	binPath := writeCwdRecordingHarness(t, recordTo)

	proc, err := StartProcess(t.Context(), binPath, &store.Session{SessionID: "sess-inherit"}, "", "")
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer proc.Kill()

	got := waitForRecordedCwd(t, recordTo)
	if want := resolvePath(t, mustGetwd(t)); got != want {
		t.Errorf("harness ran in %q, want the inherited directory %q", got, want)
	}
}

func TestVerifyLocalWorkingDirRefusesWhatCannotBeEntered(t *testing.T) {
	realDir := t.TempDir()
	aFile := filepath.Join(realDir, "regular-file")
	if err := os.WriteFile(aFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	t.Run("an existing directory is accepted", func(t *testing.T) {
		if err := verifyLocalWorkingDir("inst-1", realDir); err != nil {
			t.Errorf("verifyLocalWorkingDir(%q) = %v, want nil", realDir, err)
		}
	})

	t.Run("an empty directory is accepted, meaning inherit", func(t *testing.T) {
		if err := verifyLocalWorkingDir("inst-1", ""); err != nil {
			t.Errorf("verifyLocalWorkingDir(\"\") = %v, want nil", err)
		}
	})

	t.Run("a missing directory is refused and the error names the instance", func(t *testing.T) {
		missing := filepath.Join(realDir, "no-such-dir")
		err := verifyLocalWorkingDir("inst-cc-local", missing)
		if err == nil {
			t.Fatalf("verifyLocalWorkingDir(%q) = nil, want an error", missing)
		}
		// The operator has to edit the instance, so the instance is the thing
		// the message must name; exec's own chdir error names only the path.
		if !strings.Contains(err.Error(), "inst-cc-local") {
			t.Errorf("error %q does not name the instance", err)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name the directory", err)
		}
	})

	t.Run("a regular file is refused", func(t *testing.T) {
		err := verifyLocalWorkingDir("inst-1", aFile)
		if err == nil {
			t.Fatalf("verifyLocalWorkingDir(%q) = nil, want an error", aFile)
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error %q does not say the path is not a directory", err)
		}
	})
}

// TestThePTYSidecarIsToldTheSameDirectoryTheChildGets guards the pairing that
// makes PTY telemetry work: the sidecar tails the rollout file under the
// child's directory, so the two must not be resolved independently.
func TestThePTYSidecarIsToldTheSameDirectoryTheChildGets(t *testing.T) {
	t.Run("a configured working directory is handed straight to the sidecar", func(t *testing.T) {
		if got := ptyRolloutCwd("/srv/project"); got != "/srv/project" {
			t.Errorf("ptyRolloutCwd = %q, want the child's own working directory %q", got, "/srv/project")
		}
	})

	t.Run("with nothing configured the sidecar follows the inherited directory", func(t *testing.T) {
		want := mustGetwd(t)
		if got := ptyRolloutCwd(""); got != want {
			t.Errorf("ptyRolloutCwd = %q, want bridge-server's own directory %q", got, want)
		}
	})
}

// writeCwdRecordingHarness returns the path to an executable that writes the
// directory it was started in to recordTo, then blocks so the caller can
// manage its lifetime. It ignores the JSON-RPC "start" request on stdin;
// StartProcess only writes that request and never waits for a reply.
func writeCwdRecordingHarness(t *testing.T, recordTo string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "fake-harness")
	script := "#!/bin/sh\npwd > " + recordTo + "\nexec sleep 30\n"
	if err := os.WriteFile(binPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return binPath
}

func waitForRecordedCwd(t *testing.T, recordTo string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(recordTo)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake harness never recorded its working directory at %s", recordTo)
	return ""
}

func resolvePath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return resolved
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}
