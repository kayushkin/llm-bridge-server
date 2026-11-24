package diskusage

import (
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func exampleLayout() MachineLayout {
	return MachineLayout{
		SystemRoot:                 `C:\Windows`,
		ProgramFilesDirectories:    []string{`C:\Program Files`, `C:\Program Files (x86)`},
		ProgramData:                `C:\ProgramData`,
		ProfilesDirectory:          `C:\Users`,
		UserProfiles:               []string{`C:\Users\person`, `C:\Users\Public`, `C:\Users\Default`},
		OneDriveDirectories:        []string{`C:\Users\person\OneDrive`},
		WSLDistributionDirectories: []string{`C:\Users\person\AppData\Local\Packages\CanonicalGroupLimited.Ubuntu_79rhkp1fndgsc\LocalState`, `D:\WSL\Ubuntu`},
		HelperDirectory:            `C:\Users\person\AppData\Local\llm-bridge`,
	}
}

func TestCheckPathRulesRefusesWhatWindowsNeeds(t *testing.T) {
	refused := []struct {
		path string
		kind msg.DiskUsageEntryKind
		why  string
	}{
		{`C:\`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`D:\`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`C:`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`C:\Windows`, msg.DiskUsageEntryDirectory, "Windows itself"},
		{`c:\windows\System32\drivers`, msg.DiskUsageEntryDirectory, "Windows itself"},
		{`C:\Windows\Temp\big.log`, msg.DiskUsageEntryFile, "Windows itself"},
		{`C:\Program Files`, msg.DiskUsageEntryDirectory, "expect to exist"},
		{`C:\Program Files (x86)`, msg.DiskUsageEntryDirectory, "expect to exist"},
		{`C:\ProgramData`, msg.DiskUsageEntryDirectory, "holds C:\\ProgramData\\Microsoft"},
		{`C:\ProgramData\Microsoft\Windows Defender`, msg.DiskUsageEntryDirectory, "Windows' own program data"},
		{`C:\Users`, msg.DiskUsageEntryDirectory, "holds"},
		{`C:\Users\person`, msg.DiskUsageEntryDirectory, "holds"},
		{`C:\Users\Public`, msg.DiskUsageEntryDirectory, "expect to exist"},
		{`C:\Users\person\Downloads`, msg.DiskUsageEntryDirectory, "expect to exist"},
		{`C:\Users\person\AppData`, msg.DiskUsageEntryDirectory, "holds"},
		{`C:\Users\person\AppData\Local`, msg.DiskUsageEntryDirectory, "holds"},
		{`C:\Users\person\AppData\Local\Packages`, msg.DiskUsageEntryDirectory, "WSL"},
		{`C:\Users\person\AppData\Local\Packages\CanonicalGroupLimited.Ubuntu_79rhkp1fndgsc\LocalState\ext4.vhdx`, msg.DiskUsageEntryFile, "WSL"},
		{`D:\WSL`, msg.DiskUsageEntryDirectory, "WSL"},
		{`C:\Users\person\AppData\Local\llm-bridge\llm-bridge-disk-usage.exe`, msg.DiskUsageEntryFile, "runner's own"},
		{`C:\Users\person\OneDrive\Documents\taxes.pdf`, msg.DiskUsageEntryFile, "OneDrive"},
		{`C:\$Recycle.Bin`, msg.DiskUsageEntryDirectory, "starts with $"},
		{`D:\$Recycle.Bin\S-1-5-21\$R123.zip`, msg.DiskUsageEntryFile, "starts with $"},
		{`C:\System Volume Information`, msg.DiskUsageEntryDirectory, "top of the drive"},
		{`E:\system volume information\x`, msg.DiskUsageEntryFile, "top of the drive"},
		{`C:\pagefile.sys`, msg.DiskUsageEntryFile, "top of the drive"},
		{`C:\hiberfil.sys`, msg.DiskUsageEntryFile, "top of the drive"},
		{`C:\Windows.old`, msg.DiskUsageEntryDirectory, "top of the drive"},
		{`C:\Documents and Settings`, msg.DiskUsageEntryDirectory, "top of the drive"},
		{`D:\Games\..\..`, msg.DiskUsageEntryDirectory, "read differently"},
		{`D:\Games\.`, msg.DiskUsageEntryDirectory, "read differently"},
		{`D:\Games\Old.`, msg.DiskUsageEntryDirectory, "read differently"},
		{`D:\Games\Old `, msg.DiskUsageEntryDirectory, "read differently"},
		{`D:\Games\*`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`D:/Games`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`D:\Games\`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`D:\Games\\Old`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`\\?\D:\Games`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`\\server\share\x`, msg.DiskUsageEntryDirectory, "not a plain path"},
		{`D:\Games\file.txt:stream`, msg.DiskUsageEntryFile, "not a plain path"},
		{`D:\Games\Link`, msg.DiskUsageEntryLink, "only a file or a folder"},
		{`D:\Games`, msg.DiskUsageEntrySmallerItems, "only a file or a folder"},
	}
	for _, test := range refused {
		verdict := CheckPathRules(exampleLayout(), test.path, test.kind)
		if verdict.Allowed {
			t.Errorf("%s: allowed, want refused (%s)", test.path, test.why)
			continue
		}
		if !strings.Contains(verdict.RefusalReason, test.why) {
			t.Errorf("%s: refused with %q, want a reason mentioning %q", test.path, verdict.RefusalReason, test.why)
		}
	}
}

func TestCheckPathRulesAllowsOrdinaryThingsWithWarnings(t *testing.T) {
	allowed := []struct {
		path        string
		kind        msg.DiskUsageEntryKind
		warningHint string // "" for no warning
	}{
		{`D:\Games`, msg.DiskUsageEntryDirectory, ""},
		{`D:\Games\Old Game (2019)`, msg.DiskUsageEntryDirectory, ""},
		{`C:\Users\person\Downloads\installer.iso`, msg.DiskUsageEntryFile, ""},
		{`C:\Users\person\Videos\Recordings`, msg.DiskUsageEntryDirectory, ""},
		{`C:\Users\person\Documents\old backup`, msg.DiskUsageEntryDirectory, ""},
		{`C:\Users\person\OneDriveOld`, msg.DiskUsageEntryDirectory, ""},
		{`C:\Windows2`, msg.DiskUsageEntryDirectory, ""},
		{`C:\Program Files\Some App`, msg.DiskUsageEntryDirectory, "uninstall"},
		{`C:\ProgramData\Package Cache`, msg.DiskUsageEntryDirectory, "uninstall"},
		{`C:\Users\person\AppData\Local\Temp`, msg.DiskUsageEntryDirectory, "settings"},
		{`C:\Users\person\AppData\Local\pip\cache`, msg.DiskUsageEntryDirectory, "settings"},
	}
	for _, test := range allowed {
		verdict := CheckPathRules(exampleLayout(), test.path, test.kind)
		if !verdict.Allowed {
			t.Errorf("%s: refused (%s), want allowed", test.path, verdict.RefusalReason)
			continue
		}
		joined := strings.Join(verdict.Warnings, " ")
		if test.warningHint == "" && joined != "" {
			t.Errorf("%s: warned %q, want no warning", test.path, joined)
		}
		if test.warningHint != "" && !strings.Contains(joined, test.warningHint) {
			t.Errorf("%s: warned %q, want a warning mentioning %q", test.path, joined, test.warningHint)
		}
	}
}

func TestCheckPathRulesRefusesEverythingWithAnIncompleteLayout(t *testing.T) {
	layout := exampleLayout()
	layout.SystemRoot = ""
	if verdict := CheckPathRules(layout, `D:\Games`, msg.DiskUsageEntryDirectory); verdict.Allowed {
		t.Fatal("allowed a deletion without knowing where Windows is")
	}
	layout = exampleLayout()
	layout.UserProfiles = nil
	if verdict := CheckPathRules(layout, `D:\Games`, msg.DiskUsageEntryDirectory); verdict.Allowed {
		t.Fatal("allowed a deletion without knowing the user profiles")
	}
}

func TestCleanWindowsPath(t *testing.T) {
	for reported, want := range map[string]string{
		`\\?\C:\Users\`: `C:\Users`,
		`c:\x`:          `C:\x`,
		`C:\`:           `C:`,
		`\\server\x`:    "",
		``:              "",
	} {
		if got := CleanWindowsPath(reported); got != want {
			t.Errorf("CleanWindowsPath(%q) = %q, want %q", reported, got, want)
		}
	}
}
