//go:build windows

package diskusage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ReadMachineLayout asks Windows where it keeps what it cannot lose. A part
// it cannot read is an error, and the caller deletes nothing: a rule that
// does not know where Windows is cannot protect it.
func ReadMachineLayout() (MachineLayout, error) {
	var layout MachineLayout
	systemRoot, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		return layout, fmt.Errorf("find the Windows folder: %w", err)
	}
	layout.SystemRoot = CleanWindowsPath(systemRoot)

	for _, variable := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if value := CleanWindowsPath(os.Getenv(variable)); value != "" && !containsFold(layout.ProgramFilesDirectories, value) {
			layout.ProgramFilesDirectories = append(layout.ProgramFilesDirectories, value)
		}
	}
	if len(layout.ProgramFilesDirectories) == 0 {
		return layout, fmt.Errorf("%%ProgramFiles%% is not set")
	}
	layout.ProgramData = CleanWindowsPath(os.Getenv("ProgramData"))
	if layout.ProgramData == "" {
		return layout, fmt.Errorf("%%ProgramData%% is not set")
	}

	profiles, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`, registry.READ)
	if err != nil {
		return layout, fmt.Errorf("open the registry's list of user profiles: %w", err)
	}
	defer profiles.Close()
	profilesDirectory, err := expandedString(profiles, "ProfilesDirectory")
	if err != nil {
		return layout, fmt.Errorf("read where user profiles live: %w", err)
	}
	layout.ProfilesDirectory = CleanWindowsPath(profilesDirectory)
	for _, name := range []string{"Default", "Public"} {
		if value, err := expandedString(profiles, name); err == nil {
			layout.addProfile(value)
		}
	}
	profileKeys, err := profiles.ReadSubKeyNames(-1)
	if err != nil {
		return layout, fmt.Errorf("list user profiles: %w", err)
	}
	for _, keyName := range profileKeys {
		key, err := registry.OpenKey(profiles, keyName, registry.READ)
		if err != nil {
			return layout, fmt.Errorf("open user profile %s: %w", keyName, err)
		}
		value, err := expandedString(key, "ProfileImagePath")
		key.Close()
		if err != nil {
			continue // a profile key with no folder has nothing to protect
		}
		layout.addProfile(value)
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		layout.addProfile(userProfile)
	}

	for _, variable := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
		layout.addOneDrive(os.Getenv(variable))
	}
	if accounts, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\OneDrive\Accounts`, registry.READ); err == nil {
		names, _ := accounts.ReadSubKeyNames(-1)
		for _, name := range names {
			if key, err := registry.OpenKey(accounts, name, registry.READ); err == nil {
				if folder, err := expandedString(key, "UserFolder"); err == nil {
					layout.addOneDrive(folder)
				}
				key.Close()
			}
		}
		accounts.Close()
	}

	distributions, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Lxss`, registry.READ)
	if err != nil {
		return layout, fmt.Errorf("open the registry's list of WSL distributions: %w", err)
	}
	defer distributions.Close()
	distributionKeys, err := distributions.ReadSubKeyNames(-1)
	if err != nil {
		return layout, fmt.Errorf("list WSL distributions: %w", err)
	}
	for _, keyName := range distributionKeys {
		key, err := registry.OpenKey(distributions, keyName, registry.READ)
		if err != nil {
			return layout, fmt.Errorf("open WSL distribution %s: %w", keyName, err)
		}
		basePath, err := expandedString(key, "BasePath")
		key.Close()
		if err != nil {
			return layout, fmt.Errorf("read where WSL distribution %s keeps its disk: %w", keyName, err)
		}
		if cleaned := CleanWindowsPath(basePath); cleaned != "" {
			layout.WSLDistributionDirectories = append(layout.WSLDistributionDirectories, cleaned)
		}
	}
	if len(layout.WSLDistributionDirectories) == 0 {
		return layout, fmt.Errorf("found no WSL distribution, though the runner runs in one")
	}

	executable, err := os.Executable()
	if err != nil {
		return layout, fmt.Errorf("find this helper: %w", err)
	}
	layout.HelperDirectory = CleanWindowsPath(filepath.Dir(executable))
	return layout, layout.complete()
}

func (layout *MachineLayout) addProfile(path string) {
	if cleaned := CleanWindowsPath(path); cleaned != "" && !containsFold(layout.UserProfiles, cleaned) {
		layout.UserProfiles = append(layout.UserProfiles, cleaned)
	}
}

func (layout *MachineLayout) addOneDrive(path string) {
	if cleaned := CleanWindowsPath(path); cleaned != "" && !containsFold(layout.OneDriveDirectories, cleaned) {
		layout.OneDriveDirectories = append(layout.OneDriveDirectories, cleaned)
	}
}

func expandedString(key registry.Key, name string) (string, error) {
	value, valueType, err := key.GetStringValue(name)
	if err != nil {
		return "", err
	}
	if valueType == registry.EXPAND_SZ {
		return registry.ExpandString(value)
	}
	return value, nil
}

func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

// CheckDeletion decides whether the entry at path, which the owner saw as
// expectedKind, may be deleted from this machine: the rules, then what only
// the machine can say.
func CheckDeletion(path string, expectedKind msg.DiskUsageEntryKind) Verdict {
	layout, err := ReadMachineLayout()
	if err != nil {
		return refuse("the helper could not tell where Windows keeps what it needs (%v), so it deletes nothing", err)
	}
	verdict := CheckPathRules(layout, path, expectedKind)
	if !verdict.Allowed {
		return verdict
	}
	if err := requireFixedDrive(path[:2]); err != nil {
		return refuse("%v", err)
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
		return refuse("%s is a link (a junction or symbolic link); delete links in Explorer, where you can see what they point at", path)
	}
	if kind != expectedKind {
		return refuse("%s is now a %s, not the %s the scan found; scan again", path, kind, expectedKind)
	}
	real, err := finalPath(path)
	if err != nil {
		return refuse("could not ask Windows for the real path of %s: %v", path, err)
	}
	if !strings.EqualFold(real, path) {
		return refuse("%s is really %s (it goes through a link or a short name), so it is not deleted from here", path, real)
	}
	return verdict
}

// finalPath is the path Windows resolves path to, with every link, short
// name and differently cased part replaced by what is really on disk.
func finalPath(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(`\\?\` + path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), fileNameNormalized|volumeNameDOS)
	if err != nil {
		return "", err
	}
	if int(length) > len(buffer) {
		return "", fmt.Errorf("the path is longer than %d characters", len(buffer))
	}
	reported := windows.UTF16ToString(buffer[:length])
	if strings.HasPrefix(reported, `\\?\UNC\`) {
		return "", fmt.Errorf("it is on a network share (%s)", reported)
	}
	cleaned := CleanWindowsPath(reported)
	if cleaned == "" {
		return "", fmt.Errorf("Windows reported %q", reported)
	}
	return cleaned, nil
}

func requireFixedDrive(drive string) error {
	root, err := windows.UTF16PtrFromString(drive + `\`)
	if err != nil {
		return err
	}
	if driveType := windows.GetDriveType(root); driveType != windows.DRIVE_FIXED {
		return fmt.Errorf("%s is not a fixed local drive (Windows drive type %d), so nothing on it is scanned or deleted from here", drive, driveType)
	}
	filesystem := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return fmt.Errorf("could not read what file system %s has: %w", drive, err)
	}
	// A synced cloud drive (Google Drive for desktop mounts one) also says it
	// is fixed; deleting on it deletes from the cloud. Only a disk's own file
	// systems are scanned and deleted from.
	name := windows.UTF16ToString(filesystem)
	if name != "NTFS" && name != "ReFS" {
		return fmt.Errorf("%s has a %q file system, not NTFS or ReFS (a cloud drive, say), so nothing on it is scanned or deleted from here", drive, name)
	}
	return nil
}

// RequireFixedDrive refuses a drive the helper does not scan: anything but
// a letter and a colon, or a drive that is not fixed and local.
func RequireFixedDrive(drive string) error {
	if !driveLetter.MatchString(drive) {
		return fmt.Errorf("%q is not a drive like C:", drive)
	}
	return requireFixedDrive(drive)
}

// Drives lists the fixed local drives, with their size and free space.
func Drives() ([]msg.DiskUsageDrive, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	drives := []msg.DiskUsageDrive{}
	for letter := 'A'; letter <= 'Z'; letter++ {
		if mask&(1<<uint(letter-'A')) == 0 {
			continue
		}
		drive := string(letter) + ":"
		if requireFixedDrive(drive) != nil {
			continue
		}
		root, _ := windows.UTF16PtrFromString(drive + `\`)
		var freeToUser, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(root, &freeToUser, &total, &totalFree); err != nil {
			return nil, fmt.Errorf("read the size of %s: %w", drive, err)
		}
		label := make([]uint16, windows.MAX_PATH+1)
		if err := windows.GetVolumeInformation(root, &label[0], uint32(len(label)), nil, nil, nil, nil, 0); err != nil {
			label[0] = 0
		}
		drives = append(drives, msg.DiskUsageDrive{Drive: drive, Label: windows.UTF16ToString(label), TotalBytes: total, FreeBytes: freeToUser})
	}
	return drives, nil
}

// GetFinalPathNameByHandle flags (fileapi.h): the normalized name, starting
// with a drive letter.
const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)
