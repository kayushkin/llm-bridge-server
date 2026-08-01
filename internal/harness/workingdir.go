package harness

import (
	"fmt"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// workingDirForSession resolves the directory a harness session runs in, and
// names the record an operator would edit to change it.
//
// The cascade is four levels and it is the same rule on every transport: the
// session's own WorkingDir wins, an empty one inherits the instance's
// WorkingDir, an empty instance inherits the machine's DefaultWorkingDir, and
// an empty result means "inherit whatever the spawning process already had".
// That last level is how a machine with no configured directory has always
// behaved, so it stays spelled as the empty string rather than being resolved
// here into a guess about anyone's current directory.
//
// The owner is returned alongside the directory because it is the only part of
// the answer a failure can act on. A path that cannot be entered is reported
// back to whoever has to go and fix it, and with four levels feeding one string
// the path alone no longer says which record that is.
//
// This rule used to be written out separately at each transport that applied
// it, and the local transport was the one that never got a copy: it called
// exec.Command and never set cmd.Dir, so a working directory configured on a
// local instance was accepted by the API, stored, shown in the UI, and then
// silently ignored at spawn while ssh and runner both honoured theirs. Every
// level added since is added here, to this one function, for that reason.
func workingDirForSession(sess *store.Session, inst *msg.Instance) (dir, owner string) {
	if sess != nil && sess.WorkingDir != "" {
		return sess.WorkingDir, "session " + sess.SessionID
	}
	if inst == nil {
		return "", ""
	}
	if inst.WorkingDir != "" {
		return inst.WorkingDir, "instance " + inst.ID
	}
	if inst.Machine != nil && inst.Machine.DefaultWorkingDir != "" {
		return inst.Machine.DefaultWorkingDir, "machine " + inst.Machine.Name
	}
	return "", ""
}

// ptyRolloutCwd returns the directory the OTel sidecar should tail a PTY
// session's rollout file under.
//
// It must answer with the directory the PTY child is actually given, which is
// the session's resolved working directory whenever there is one. Only when
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
// The error names the owner workingDirForSession returned, because that is the
// record the operator has to go and edit; exec's own chdir error names only the
// path, and the path is on four records at once.
func verifyLocalWorkingDir(owner, dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s working directory %q: %w", owner, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s working directory %q is not a directory", owner, dir)
	}
	return nil
}
