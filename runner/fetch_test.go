package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// fakeBinaryServer serves one wrapper the way the bridge does (ServeContent,
// so Last-Modified and If-Modified-Since behave as in production).
type fakeBinaryServer struct {
	content     atomic.Value // []byte
	modified    atomic.Value // time.Time
	fullAnswers atomic.Int32
}

func (f *fakeBinaryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	recorder := &statusRecorder{ResponseWriter: w}
	http.ServeContent(recorder, r, "wrapper", f.modified.Load().(time.Time), bytes.NewReader(f.content.Load().([]byte)))
	if recorder.status == http.StatusOK {
		f.fullAnswers.Add(1)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func TestInstallCurrentHarnessBinaryDownloadsKeepsAndReplaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := &fakeBinaryServer{}
	server.content.Store([]byte("april wrapper"))
	server.modified.Store(time.Date(2026, 4, 26, 21, 58, 0, 0, time.UTC))
	bridge := httptest.NewServer(server)
	t.Cleanup(bridge.Close)

	path, err := installCurrentHarnessBinary(bridge.URL, msg.HarnessClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "april wrapper" {
		t.Fatalf("first install = %q", got)
	}

	// Nothing new on the bridge: the copy stays and no body is sent.
	if _, err := installCurrentHarnessBinary(bridge.URL, msg.HarnessClaudeCode); err != nil {
		t.Fatal(err)
	}
	if n := server.fullAnswers.Load(); n != 1 {
		t.Errorf("the bridge sent the wrapper %d times for an unchanged copy, want 1", n)
	}

	// A newer wrapper on the bridge replaces the copy.
	server.content.Store([]byte("october wrapper"))
	server.modified.Store(time.Date(2026, 10, 14, 22, 0, 0, 0, time.UTC))
	if _, err := installCurrentHarnessBinary(bridge.URL, msg.HarnessClaudeCode); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "october wrapper" {
		t.Errorf("after the bridge updated, the copy is %q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0755 {
		t.Errorf("mode = %v", info.Mode())
	}
}

func TestInstallCurrentBinaryReportsWhetherItReplacedTheFile(t *testing.T) {
	server := &fakeBinaryServer{}
	server.content.Store([]byte("v1"))
	server.modified.Store(time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC))
	bridge := httptest.NewServer(server)
	t.Cleanup(bridge.Close)
	dest := t.TempDir() + "/llm-bridge-runner"

	for i, want := range []bool{true, false} {
		replaced, err := installCurrentBinary(bridge.URL, dest)
		if err != nil || replaced != want {
			t.Fatalf("call %d: replaced=%v err=%v, want %v", i, replaced, err, want)
		}
	}
	server.content.Store([]byte("v2"))
	server.modified.Store(time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC))
	if replaced, err := installCurrentBinary(bridge.URL, dest); err != nil || !replaced {
		t.Fatalf("after the bridge updated: replaced=%v err=%v", replaced, err)
	}
}

// A runner run from anywhere but the installer's ~/.local/bin (a development
// build, this test binary) is never replaced, and the bridge is not asked.
func TestARunnerOutsideTheInstalledPathIsNotUpdated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	replaced, err := installCurrentRunner("http://bridge.invalid")
	if err != nil || replaced {
		t.Errorf("replaced=%v err=%v; want no update and no request", replaced, err)
	}
}

func TestNoComputerUseToolOutsideWSL(t *testing.T) {
	if runningInWSLWithInterop() {
		t.Skip("this machine is WSL")
	}
	tool, err := installCurrentComputerUseTool("http://bridge.invalid")
	if tool != nil || err != nil {
		t.Errorf("tool=%+v err=%v; want none, quietly", tool, err)
	}
}
