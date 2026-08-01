package harness

import (
	"fmt"
	"os"

	"github.com/kayushkin/llm-bridge/msg"
)

// workingDirForInstance resolves the directory a harness session runs in.
//
// The cascade is two levels and it is the same rule on every transport: the
// instance's own WorkingDir wins, and an empty one inherits the machine's
// DefaultWorkingDir. An empty result means "inherit whatever the spawning
// process already had", which is how a machine with no configured directory
// has always behaved — so it stays spelled as the empty string rather than
// being resolved here into a guess about anyone's current directory.
//
// This rule used to be written out separately at each transport that applied
// it, and the local transport was the one that never got a copy: it called
// exec.Command and never set cmd.Dir, so a working directory configured on a
// local instance was accepted by the API, stored, shown in the UI, and then
// silently ignored at spawn while ssh and runner both honoured theirs.
func workingDirForInstance(inst *msg.Instance) string {
	if inst == nil {
		return ""
	}
	if inst.WorkingDir != "" {
		return inst.WorkingDir
	}
	if inst.Machine != nil {
		return inst.Machine.DefaultWorkingDir
	}
	return ""
}

// ptyRolloutCwd returns the directory the OTel sidecar should tail a PTY
// session's rollout file under.
//
// It must answer with the directory the PTY child is actually given, which is
// the instance's resolved working directory whenever there is one. Only when
// nothing is configured does the child inherit bridge-server's own directory,
// and only then does the sidecar have to go and ask what that is.
//
// The two are one fact reached by two routes, and they used to disagree: the
// child's directory came from the instance (or, before this was fixed, from
// nowhere at all) while the sidecar always got os.Getwd(). A sidecar pointed
// at a directory the child is not writing in tails a rollout file that never
// appears, and the session simply produces no telemetry.
func ptyRolloutCwd(workingDir string) string {
	if workingDir != "" {
		return workingDir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "/"
}

// verifyLocalWorkingDir checks that a resolved working directory can actually
// be entered by a subprocess on this host.
//
// Only the local transport may ask this. An ssh or runner working directory
// names a path on another machine, where this process's filesystem answers a
// different question entirely — an existing local /home/agent would vouch for
// a remote path that isn't there, and a missing one would refuse a remote path
// that is. Those transports therefore pass their directory through unchecked
// and let the remote side report its own failure.
//
// The error names the instance because that is the thing the operator has to
// go and edit; exec's own chdir error names only the path.
func verifyLocalWorkingDir(instanceID, dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("instance %s working directory %q: %w", instanceID, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("instance %s working directory %q is not a directory", instanceID, dir)
	}
	return nil
}
