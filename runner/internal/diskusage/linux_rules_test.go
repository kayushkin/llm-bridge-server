package diskusage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func exampleLinuxLayout() LinuxLayout {
	return LinuxLayout{
		HomeDirectory: "/home/person",
		RunnerPaths:   []string{"/home/person/.local/bin/llm-bridge-runner", "/home/person/.config/llm-bridge-runner", "/home/person/.cache/llm-bridge-runner"},
	}
}

func TestCheckLinuxPathRules(t *testing.T) {
	refused := map[string]string{
		"/":                            "runner's own",
		"/home":                        "runner's own",
		"/home/person":                 "runner's own",
		"/home/other/builds":           "neither",
		"/usr/lib":                     "neither",
		"/etc/passwd":                  "neither",
		"/mnt/c/Users":                 "neither",
		"/tmp":                         "neither",
		"/home/person/.ssh":            "keys",
		"/home/person/.ssh/id_ed25519": "keys",
		"/home/person/.gnupg/x":        "keys",
		"/home/person/.local/bin/llm-bridge-runner":          "runner's own",
		"/home/person/.local/bin":                            "runner's own",
		"/home/person/.config/llm-bridge-runner/config.json": "runner's own",
		"/home/person/.config":                               "runner's own",
		"/home/person/.cache":                                "runner's own",
		"/home/person/.local/share":                          "expect to exist",
		"/home/person/.claude":                               "expect to exist",
		"/home/person/builds/../.ssh":                        "plain absolute",
		"/home/person/builds/":                               "plain absolute",
		"home/person/builds":                                 "plain absolute",
		"/home/person//builds":                               "plain absolute",
	}
	for path, why := range refused {
		verdict := CheckLinuxPathRules(exampleLinuxLayout(), path, msg.DiskUsageEntryDirectory)
		if verdict.Allowed {
			t.Errorf("%s: allowed, want refused (%s)", path, why)
		} else if !strings.Contains(verdict.RefusalReason, why) {
			t.Errorf("%s: refused with %q, want a reason mentioning %q", path, verdict.RefusalReason, why)
		}
	}
	allowed := map[string]string{
		"/home/person/remote-builds/game-wt-old": "",
		"/home/person/.cache/pip":                "",
		"/home/person/.rustup/toolchains/old":    "",
		"/tmp/build-output":                      "",
		"/var/tmp/x.iso":                         "",
		"/home/person/.local/share/Trash":        "settings",
		"/home/person/.config/old-app":           "settings",
	}
	for path, hint := range allowed {
		verdict := CheckLinuxPathRules(exampleLinuxLayout(), path, msg.DiskUsageEntryDirectory)
		if !verdict.Allowed {
			t.Errorf("%s: refused (%s), want allowed", path, verdict.RefusalReason)
			continue
		}
		joined := strings.Join(verdict.Warnings, " ")
		if (hint == "") != (joined == "") || !strings.Contains(joined, hint) {
			t.Errorf("%s: warnings %q, want hint %q", path, joined, hint)
		}
	}
	if verdict := CheckLinuxPathRules(LinuxLayout{HomeDirectory: "/home/person"}, "/home/person/x", msg.DiskUsageEntryFile); verdict.Allowed {
		t.Error("allowed a deletion without knowing the runner's own files")
	}
	if verdict := CheckLinuxPathRules(exampleLinuxLayout(), "/home/person/x", msg.DiskUsageEntryMountPoint); verdict.Allowed {
		t.Error("allowed deleting a mount point")
	}
}

func TestCheckLinuxDeletionOnThisMachine(t *testing.T) {
	home := t.TempDir()
	layout := LinuxLayout{HomeDirectory: home, RunnerPaths: []string{filepath.Join(home, ".config", "llm-bridge-runner")}}
	device, err := DeviceOfPath(home)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "builds", "a.bin"), 100)
	writeFile(t, filepath.Join(home, "elsewhere", "b.bin"), 100)
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(home, "linked")); err != nil {
		t.Fatal(err)
	}

	if verdict := CheckLinuxDeletion(layout, device, filepath.Join(home, "builds"), msg.DiskUsageEntryDirectory); !verdict.Allowed {
		t.Fatalf("refused an ordinary folder: %s", verdict.RefusalReason)
	}
	for path, why := range map[string]string{
		filepath.Join(home, "builds"):          "now a directory",
		filepath.Join(home, "linked"):          "symbolic link",
		filepath.Join(home, "linked", "b.bin"): "really",
		filepath.Join(home, "missing"):         "not there",
	} {
		kind := msg.DiskUsageEntryFile
		if strings.HasSuffix(path, "linked") {
			kind = msg.DiskUsageEntryDirectory
		}
		if verdict := CheckLinuxDeletion(layout, device, path, kind); verdict.Allowed || !strings.Contains(verdict.RefusalReason, why) {
			t.Errorf("%s: %+v, want a refusal mentioning %q", path, verdict, why)
		}
	}
	if verdict := CheckLinuxDeletion(layout, device+1, filepath.Join(home, "builds"), msg.DiskUsageEntryDirectory); verdict.Allowed || !strings.Contains(verdict.RefusalReason, "another file system") {
		t.Errorf("another device: %+v", verdict)
	}

	// A program working inside the folder keeps it.
	sleeper := exec.Command("sleep", "30")
	sleeper.Dir = filepath.Join(home, "builds")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sleeper.Process.Kill(); sleeper.Wait() }()
	time.Sleep(100 * time.Millisecond)
	if verdict := CheckLinuxDeletion(layout, device, filepath.Join(home, "builds"), msg.DiskUsageEntryDirectory); verdict.Allowed || !strings.Contains(verdict.RefusalReason, "sleep") {
		t.Errorf("a folder with a program in it: %+v", verdict)
	}
}

func TestScanStaysOnItsDevice(t *testing.T) {
	// /dev/pts is its own file system inside /dev on Linux: a scan of /dev
	// that stays on its device must mark it a mount point and not read it.
	devDevice, err := DeviceOfPath("/dev")
	if err != nil {
		t.Skip("no /dev here")
	}
	ptsDevice, err := DeviceOfPath("/dev/pts")
	if err != nil || ptsDevice == devDevice {
		t.Skip("/dev/pts is not a separate file system here")
	}
	scanner := &Scanner{Keep: KeepPolicy{Largest: 5000, AlwaysKept: 5000}, Parallelism: 2, StayOnDevice: true}
	tree, err := scanner.Scan("/dev", "/dev")
	if err != nil {
		t.Fatal(err)
	}
	pts, _ := tree.Find("/dev/", "/dev/pts")
	if pts == nil || pts.Kind != msg.DiskUsageEntryMountPoint || pts.SizeBytes != 0 || len(pts.Children) != 0 {
		t.Fatalf("/dev/pts: %+v", pts)
	}
}

func TestScanCountsDiskBlocksAndHardLinksOnce(t *testing.T) {
	root := t.TempDir()
	// A sparse file: 100 MB long, almost nothing on disk.
	sparse, err := os.Create(filepath.Join(root, "sparse.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(100 << 20); err != nil {
		t.Fatal(err)
	}
	sparse.Close()
	writeFile(t, filepath.Join(root, "real.bin"), 64<<10)
	if err := os.Link(filepath.Join(root, "real.bin"), filepath.Join(root, "real-link.bin")); err != nil {
		t.Fatal(err)
	}
	byLength, err := (&Scanner{Keep: DefaultKeepPolicy, Parallelism: 1}).Scan(root, "/")
	if err != nil {
		t.Fatal(err)
	}
	byBlocks, err := (&Scanner{Keep: DefaultKeepPolicy, Parallelism: 1, CountDiskBlocks: true}).Scan(root, "/")
	if err != nil {
		t.Fatal(err)
	}
	if byLength.SizeBytes != 100<<20+2*(64<<10) {
		t.Fatalf("by length %d", byLength.SizeBytes)
	}
	if byBlocks.SizeBytes < 64<<10 || byBlocks.SizeBytes > 64<<10+64<<10 {
		t.Fatalf("by blocks %d, want the one real file's 64 KiB once, give or take a block", byBlocks.SizeBytes)
	}
	if byBlocks.FileCount != 3 {
		t.Fatalf("file count %d, want every name counted", byBlocks.FileCount)
	}
}
