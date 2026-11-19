package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// DetectHarnesses scans PATH for the wrapper binaries of every known
// harness type and returns the ones present. Used to populate
// Hello.AvailableHarnesses so the server learns what this machine can run.
func DetectHarnesses() []msg.HarnessAvailable {
	var out []msg.HarnessAvailable
	for _, h := range msg.AllHarnesses {
		bin := msg.HarnessBinaryName(h)
		if bin == "" {
			continue
		}
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		out = append(out, msg.HarnessAvailable{
			Harness: h,
			Binary:  path,
			Version: harnessVersion(path),
		})
	}
	return out
}

// harnessVersion best-effort reports a binary's version string by running
// it with -version (or returns "" if unavailable). Bounded timeout so a
// hung binary doesn't stall startup.
func harnessVersion(binPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "-version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
