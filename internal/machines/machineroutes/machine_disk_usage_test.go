package machineroutes

import (
	"testing"
)

func TestDiskPathName(t *testing.T) {
	for path, want := range map[string]string{
		`D:\Games\Old Game`:              "Old Game",
		`C:\x.iso`:                       "x.iso",
		"/home/person/remote-builds/old": "old",
		"/tmp/a b":                       "a b",
		`/home/person/odd\name`:          `odd\name`,
		`C:\`:                            "",
		"/":                              "",
	} {
		if got := DiskPathName(path); got != want {
			t.Errorf("diskPathName(%q) = %q, want %q", path, got, want)
		}
	}
}
