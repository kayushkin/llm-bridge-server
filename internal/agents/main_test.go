package agents

import (
	"fmt"
	"os"
	"testing"
)

// homeTheTestsStartedWith is $HOME as the test binary found it, which is the
// real home of whoever runs the tests. testHomeDirectory refuses it.
var homeTheTestsStartedWith = os.Getenv("HOME")

// TestMain points HOME at a temporary folder before any test runs, so that the
// scan, the renderer and every file write work in that folder even in a test
// that sets no home of its own. The store never reads HOME itself (New is
// given its home directory), so this guards the tests' own helpers and
// anything left over that does.
func TestMain(m *testing.M) {
	temporaryHome, err := os.MkdirTemp("", "agentstore-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", temporaryHome); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(temporaryHome)
	os.Exit(code)
}
