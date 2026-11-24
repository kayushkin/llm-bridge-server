package diskusage

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// The rules for deleting inside the WSL distribution the runner runs in.
// The runner deletes there itself, as its own user, so it cannot touch what
// that user may not; these rules narrow it much further. Like the Windows
// rules they read paths as text against a layout, so they can be tested
// anywhere, and the machine's own checks follow in CheckLinuxDeletion.

// LinuxLayout is what the rules protect inside the distribution. Paths are
// absolute and clean.
type LinuxLayout struct {
	HomeDirectory string // /home/name
	// RunnerPaths are the runner's own files and folders: its binary, its
	// configuration, its logs. Neither they nor what holds them is deleted.
	RunnerPaths []string
}

// Folders under the home folder that are never deleted, nor anything in them:
// the keys the runner's SSH tunnel and git use.
var homeFoldersNeverTouched = []string{".ssh", ".gnupg"}

// Folders under the home folder whose folder is kept, though what is inside
// them may go.
var homeFoldersKept = []string{".config", ".local", ".local/share", ".local/bin", ".local/state", ".claude", ".codex"}

// Places outside the home folder where things may be deleted.
var linuxTemporaryDirectories = []string{"/tmp", "/var/tmp"}

// CheckLinuxPathRules decides by the rules alone whether the entry at path
// may be deleted.
func CheckLinuxPathRules(layout LinuxLayout, path string, kind msg.DiskUsageEntryKind) Verdict {
	if kind != msg.DiskUsageEntryFile && kind != msg.DiskUsageEntryDirectory {
		return refuse("only a file or a folder can be deleted here, and this is a %s", kind)
	}
	if !strings.HasPrefix(path, "/") || pathpkg.Clean(path) != path || strings.ContainsRune(path, 0) {
		return refuse("%q is not a plain absolute path", path)
	}
	if layout.HomeDirectory == "" || !strings.HasPrefix(layout.HomeDirectory, "/") || pathpkg.Clean(layout.HomeDirectory) != layout.HomeDirectory || layout.HomeDirectory == "/" {
		return refuse("the runner could not tell where its home folder is (%q), so it deletes nothing", layout.HomeDirectory)
	}
	if len(layout.RunnerPaths) == 0 {
		return refuse("the runner could not tell where its own files are, so it deletes nothing")
	}

	for _, runnerPath := range layout.RunnerPaths {
		if isWithinLinux(path, runnerPath) || isWithinLinux(runnerPath, path) {
			return refuse("%s is, holds, or is inside the runner's own files (%s)", path, runnerPath)
		}
	}
	for _, name := range homeFoldersNeverTouched {
		folder := layout.HomeDirectory + "/" + name
		if isWithinLinux(path, folder) {
			return refuse("%s is inside %s, which holds the keys SSH and git use; it is never deleted from here", path, folder)
		}
	}

	inHome := isWithinLinux(path, layout.HomeDirectory) && path != layout.HomeDirectory
	inTemporary := false
	for _, directory := range linuxTemporaryDirectories {
		if isWithinLinux(path, directory) && path != directory {
			inTemporary = true
		}
	}
	if !inHome && !inTemporary {
		return refuse("only things inside your home folder (%s) or the temporary folders (%s) can be deleted from here; %s is neither", layout.HomeDirectory, strings.Join(linuxTemporaryDirectories, ", "), path)
	}
	for _, name := range homeFoldersKept {
		if path == layout.HomeDirectory+"/"+name {
			return refuse("%s is a folder programs expect to exist; delete what is inside it instead", path)
		}
	}

	verdict := Verdict{Allowed: true}
	for _, name := range []string{".config", ".local/share", ".claude", ".codex"} {
		folder := layout.HomeDirectory + "/" + name
		if isWithinLinux(path, folder) {
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf("This is inside %s, where programs keep their settings and data. The program it belongs to may lose them; close it first.", folder))
		}
	}
	return verdict
}

// isWithinLinux reports whether path is directory or inside it.
func isWithinLinux(path, directory string) bool {
	if directory == "" {
		return false
	}
	if path == directory {
		return true
	}
	if directory == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, directory+"/")
}

// ReadLinuxLayout reads the runner's home folder and its own paths.
func ReadLinuxLayout(runnerPaths []string) (LinuxLayout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return LinuxLayout{}, err
	}
	layout := LinuxLayout{HomeDirectory: filepath.Clean(home)}
	for _, runnerPath := range runnerPaths {
		if runnerPath != "" {
			layout.RunnerPaths = append(layout.RunnerPaths, filepath.Clean(runnerPath))
		}
	}
	return layout, nil
}

// CheckLinuxDeletion decides whether the entry at path, which the owner saw
// as expectedKind, may be deleted: the rules, then what only the machine can
// say — it exists and is still that kind, it is no link, nothing on its way
// is a link, it is on the same file system as rootDevice, and no program is
// working inside it.
func CheckLinuxDeletion(layout LinuxLayout, rootDevice uint64, path string, expectedKind msg.DiskUsageEntryKind) Verdict {
	verdict := CheckLinuxPathRules(layout, path, expectedKind)
	if !verdict.Allowed {
		return verdict
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return refuse("%s is not there any more; scan again", path)
	}
	if err != nil {
		return refuse("could not look at %s: %v", path, err)
	}
	kind, _ := classify(info)
	if kind == msg.DiskUsageEntryLink {
		return refuse("%s is a symbolic link; remove links by hand, where you can see what they point at", path)
	}
	if kind != expectedKind {
		return refuse("%s is now a %s, not the %s the scan found; scan again", path, kind, expectedKind)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return refuse("could not resolve the real path of %s: %v", path, err)
	}
	if real != path {
		return refuse("%s is really %s (a folder on its way is a link), so it is not deleted from here", path, real)
	}
	device, ok := deviceOf(info)
	if !ok || device != rootDevice {
		return refuse("%s is on another file system than WSL's own (a Windows drive, say), so it is not deleted from here", path)
	}
	if busy, err := processesWorkingIn(path); err != nil {
		return refuse("could not check which programs are working in %s: %v", path, err)
	} else if len(busy) > 0 {
		return refuse("programs are working inside %s (%s); stop them first", path, strings.Join(busy, ", "))
	}
	return verdict
}

// processesWorkingIn lists the processes, other than this one, whose
// working directory is path or inside it, as "name (pid)". It reads /proc,
// so it sees only processes of this user and those it may inspect.
func processesWorkingIn(path string) ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var busy []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		cwd, err := os.Readlink("/proc/" + entry.Name() + "/cwd")
		if err != nil {
			continue // gone, or not ours to read
		}
		if isWithinLinux(cwd, path) {
			name, _ := os.ReadFile("/proc/" + entry.Name() + "/comm")
			busy = append(busy, fmt.Sprintf("%s (%d)", strings.TrimSpace(string(name)), pid))
		}
	}
	return busy, nil
}

// DeviceOfPath is the device path lives on.
func DeviceOfPath(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	device, ok := deviceOf(info)
	if !ok {
		return 0, fmt.Errorf("cannot tell which device %s is on", path)
	}
	return device, nil
}
