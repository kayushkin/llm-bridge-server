package diskusage

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// The rules for what may be deleted. They read paths as text, in Windows
// form, against a MachineLayout the helper reads from Windows itself, so
// they can be tested anywhere. The helper adds the checks only the machine
// can answer (does it exist, is it what the scan said, is it a link, does
// its real path match) in its own code.

// MachineLayout is where a Windows machine keeps what it cannot lose.
// Every path is absolute, in `C:\Folder` form, with no trailing backslash.
type MachineLayout struct {
	SystemRoot              string   // C:\Windows
	ProgramFilesDirectories []string // C:\Program Files, C:\Program Files (x86)
	ProgramData             string   // C:\ProgramData
	ProfilesDirectory       string   // C:\Users
	UserProfiles            []string // C:\Users\name, C:\Users\Public, C:\Users\Default
	// OneDriveDirectories are synced folders: deleting in one deletes the
	// files from the cloud and from every other computer too.
	OneDriveDirectories []string
	// WSLDistributionDirectories hold each WSL distribution's disk. The
	// runner itself lives on one.
	WSLDistributionDirectories []string
	// HelperDirectory is where the runner keeps this helper and the
	// computer-use tool.
	HelperDirectory string
}

// Verdict is the answer to "may this be deleted?".
type Verdict struct {
	Allowed       bool     `json:"allowed"`
	RefusalReason string   `json:"refusal_reason,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func refuse(format string, arguments ...any) Verdict {
	return Verdict{RefusalReason: fmt.Sprintf(format, arguments...)}
}

// Root entries Windows keeps on any drive. Neither they nor anything inside
// them may be deleted. Any root entry whose name starts with "$" is
// refused as well.
var rootEntriesWindowsKeeps = []string{
	"System Volume Information", "Recovery", "Boot", "EFI", "Config.Msi",
	"Windows.old", "Documents and Settings", "MSOCache", "OneDriveTemp",
	"pagefile.sys", "hiberfil.sys", "swapfile.sys", "DumpStack.log", "DumpStack.log.tmp",
	"bootmgr", "BOOTNXT", "BOOTSECT.BAK",
}

// Folders inside each user profile whose folder may not be deleted, though
// what is inside them may.
var profileFoldersKept = []string{
	"AppData", `AppData\Local`, `AppData\LocalLow`, `AppData\Roaming`,
	"Desktop", "Documents", "Downloads", "Pictures", "Music", "Videos",
	"Saved Games", "Favorites", "Contacts", "Links", "Searches", "3D Objects",
}

var driveLetter = regexp.MustCompile(`^[A-Z]:$`)

var plainWindowsPath = regexp.MustCompile(`^[A-Za-z]:\\[^\\/:*?"<>|\x00-\x1f]+(\\[^\\/:*?"<>|\x00-\x1f]+)*$`)

// CheckPathRules decides by the rules alone whether the entry at path, of
// the given kind, may be deleted from the machine laid out as layout.
func CheckPathRules(layout MachineLayout, path string, kind msg.DiskUsageEntryKind) Verdict {
	if kind != msg.DiskUsageEntryFile && kind != msg.DiskUsageEntryDirectory {
		return refuse("only a file or a folder can be deleted here, and this is a %s", kind)
	}
	if !plainWindowsPath.MatchString(path) {
		return refuse("%q is not a plain path on a drive (like D:\\Games\\Old)", path)
	}
	components := strings.Split(path, `\`)
	for _, component := range components[1:] {
		if strings.Trim(component, ". ") == "" || strings.HasSuffix(component, " ") || strings.HasSuffix(component, ".") {
			return refuse("%q has a part Windows would read differently from how it is written", path)
		}
	}
	if err := layout.complete(); err != nil {
		return refuse("the helper could not tell where Windows keeps what it needs (%v), so it deletes nothing", err)
	}

	rootEntry := components[1]
	if strings.HasPrefix(rootEntry, "$") {
		return refuse("%s holds something Windows keeps at the top of the drive (its name starts with $)", path)
	}
	for _, kept := range rootEntriesWindowsKeeps {
		if strings.EqualFold(rootEntry, kept) {
			return refuse("%s is, or is inside, %s, which Windows keeps at the top of the drive", path, components[0]+`\`+kept)
		}
	}

	type neverInside struct {
		directory, why string
	}
	var never []neverInside
	never = append(never, neverInside{layout.SystemRoot, "Windows itself; free space in it with Disk Cleanup instead"})
	never = append(never, neverInside{layout.ProgramData + `\Microsoft`, "Windows' own program data"})
	for _, directory := range layout.WSLDistributionDirectories {
		never = append(never, neverInside{directory, "a WSL distribution's disk, which the runner runs on"})
	}
	never = append(never, neverInside{layout.HelperDirectory, "the runner's own Windows tools"})
	for _, directory := range layout.OneDriveDirectories {
		never = append(never, neverInside{directory, "a OneDrive folder: deleting here deletes the files from the cloud and every other computer too"})
	}
	for _, rule := range never {
		if isWithinOrSame(path, rule.directory) {
			return refuse("%s is inside %s, %s", path, rule.directory, rule.why)
		}
		if isWithinOrSame(rule.directory, path) {
			return refuse("%s holds %s, %s", path, rule.directory, rule.why)
		}
	}

	var kept []string
	kept = append(kept, layout.ProgramFilesDirectories...)
	kept = append(kept, layout.ProgramData, layout.ProfilesDirectory)
	for _, profile := range layout.UserProfiles {
		kept = append(kept, profile)
		for _, folder := range profileFoldersKept {
			kept = append(kept, profile+`\`+folder)
		}
	}
	for _, directory := range kept {
		if isWithinOrSame(directory, path) {
			if strings.EqualFold(directory, path) {
				return refuse("%s is a folder Windows or its programs expect to exist; delete what is inside it instead", path)
			}
			return refuse("%s holds %s, a folder Windows or its programs expect to exist", path, directory)
		}
	}

	verdict := Verdict{Allowed: true}
	for _, directory := range append(append([]string{}, layout.ProgramFilesDirectories...), layout.ProgramData) {
		if isWithinOrSame(path, directory) {
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf("This is inside %s. An installed program may need it; if it belongs to one, uninstall that program from Settings → Apps instead.", directory))
		}
	}
	for _, profile := range layout.UserProfiles {
		appData := profile + `\AppData`
		if isWithinOrSame(path, appData) {
			verdict.Warnings = append(verdict.Warnings, fmt.Sprintf("This is inside %s, where programs keep their settings and data. The program it belongs to may lose its settings or stop working; close it first.", appData))
		}
	}
	return verdict
}

func (layout MachineLayout) complete() error {
	switch {
	case layout.SystemRoot == "":
		return fmt.Errorf("no Windows folder")
	case len(layout.ProgramFilesDirectories) == 0:
		return fmt.Errorf("no Program Files folder")
	case layout.ProgramData == "":
		return fmt.Errorf("no ProgramData folder")
	case layout.ProfilesDirectory == "":
		return fmt.Errorf("no folder of user profiles")
	case len(layout.UserProfiles) == 0:
		return fmt.Errorf("no user profiles")
	case layout.HelperDirectory == "":
		return fmt.Errorf("no folder of its own")
	}
	for _, path := range layout.all() {
		if !plainWindowsPath.MatchString(path) {
			return fmt.Errorf("%q is not a plain path", path)
		}
	}
	return nil
}

func (layout MachineLayout) all() []string {
	paths := []string{layout.SystemRoot, layout.ProgramData, layout.ProfilesDirectory, layout.HelperDirectory}
	paths = append(paths, layout.ProgramFilesDirectories...)
	paths = append(paths, layout.UserProfiles...)
	paths = append(paths, layout.OneDriveDirectories...)
	paths = append(paths, layout.WSLDistributionDirectories...)
	return paths
}

// isWithinOrSame reports whether path is directory or inside it, ignoring
// case as Windows does.
func isWithinOrSame(path, directory string) bool {
	if directory == "" {
		return false
	}
	if strings.EqualFold(path, directory) {
		return true
	}
	prefix := directory
	if !strings.HasSuffix(prefix, `\`) {
		prefix += `\`
	}
	return len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix)
}

// CleanWindowsPath turns a path Windows reported (`\\?\C:\Users\`, `c:\x`)
// into the form the rules use (`C:\Users`, `C:\x`), or "" when it is not a
// path on a drive.
func CleanWindowsPath(path string) string {
	path = strings.TrimPrefix(path, `\\?\`)
	path = strings.TrimRight(path, `\`)
	if len(path) < 2 || path[1] != ':' {
		return ""
	}
	return strings.ToUpper(path[:1]) + path[1:]
}
