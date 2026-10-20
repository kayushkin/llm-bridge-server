package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordPlaywrightBrowserVideosPointsOnlyThePlaywrightServerAtAConfig(t *testing.T) {
	videoDirectory := t.TempDir()
	bundle := `{"mcpServers":{
		"browser":{"command":"npx","args":["-y","@playwright/mcp@latest","--headless"]},
		"chrome-devtools":{"command":"npx","args":["-y","chrome-devtools-mcp@latest"]}}}`
	rewritten, recording, err := recordPlaywrightBrowserVideos(bundle, videoDirectory)
	if err != nil || !recording {
		t.Fatalf("recording=%v err=%v; want a recording bundle", recording, err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(rewritten), &parsed); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(videoDirectory, "playwright-config.json")
	if got := strings.Join(parsed.MCPServers["browser"].Args, " "); got != "-y @playwright/mcp@latest --headless --config "+configPath {
		t.Errorf("playwright args = %q", got)
	}
	if got := strings.Join(parsed.MCPServers["chrome-devtools"].Args, " "); strings.Contains(got, "--config") {
		t.Errorf("chrome-devtools was given a config: %q", got)
	}
	var config struct {
		Browser struct {
			ContextOptions struct {
				RecordVideo struct {
					Dir string `json:"dir"`
				} `json:"recordVideo"`
			} `json:"contextOptions"`
		} `json:"browser"`
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &config); err != nil || config.Browser.ContextOptions.RecordVideo.Dir != videoDirectory {
		t.Errorf("config %s records into %q (err %v); want %q", content, config.Browser.ContextOptions.RecordVideo.Dir, err, videoDirectory)
	}
}

func TestRecordPlaywrightBrowserVideosLeavesABundleWithoutPlaywrightAlone(t *testing.T) {
	bundle := `{"mcpServers":{}}`
	rewritten, recording, err := recordPlaywrightBrowserVideos(bundle, t.TempDir())
	if err != nil || recording || rewritten != bundle {
		t.Errorf("got %q recording=%v err=%v; want the bundle unchanged", rewritten, recording, err)
	}
}

func TestRecordPlaywrightBrowserVideosRefusesToReplaceAConfig(t *testing.T) {
	bundle := `{"mcpServers":{"playwright":{"command":"npx","args":["@playwright/mcp","--config","/mine.json"]}}}`
	if _, _, err := recordPlaywrightBrowserVideos(bundle, t.TempDir()); err == nil {
		t.Error("a server with its own --config was given another")
	}
}

// fakeSessionServer answers the session and instance reads the machine check
// makes, and keeps what a session file share sent.
func fakeSessionServer(t *testing.T, instanceBySession, machineByInstance map[string]string, shared *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sessions/"):
			instanceID, known := instanceBySession[strings.TrimPrefix(r.URL.Path, "/sessions/")]
			if !known {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"instance_id": instanceID})
		case r.Method == http.MethodGet && r.URL.Path == "/instances":
			var instances []map[string]string
			for id, machine := range machineByInstance {
				instances = append(instances, map[string]string{"id": id, "machine_id": machine})
			}
			json.NewEncoder(w).Encode(instances)
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			body, _ := io.ReadAll(r.Body)
			*shared = append(*shared, r.URL.Query().Get("filename")+" "+r.Header.Get("Content-Type")+" "+r.Header.Get("Authorization")+" "+string(body))
			json.NewEncoder(w).Encode(map[string]string{"file_id": "file_000001"})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	}))
}

func TestDelegateSharesThisMachineComparesTheTwoInstancesMachines(t *testing.T) {
	server := fakeSessionServer(t,
		map[string]string{"br_caller": "inst-local", "br_here": "inst-local-two", "br_away": "inst-runner"},
		map[string]string{"inst-local": "m_one", "inst-local-two": "m_one", "inst-runner": "m_two"}, nil)
	defer server.Close()
	d := &delegate{client: server.Client(), server: server.URL}
	ctx := context.Background()

	if same, reason, err := d.delegateSharesThisMachine(ctx, "br_here", "br_caller"); err != nil || !same {
		t.Errorf("same machine: same=%v reason=%q err=%v", same, reason, err)
	}
	if same, reason, err := d.delegateSharesThisMachine(ctx, "br_away", "br_caller"); err != nil || same || !strings.Contains(reason, "m_two") {
		t.Errorf("other machine: same=%v reason=%q err=%v", same, reason, err)
	}
	if same, _, err := d.delegateSharesThisMachine(ctx, "br_here", ""); err != nil || same {
		t.Errorf("no calling session: same=%v err=%v; want no", same, err)
	}
	if _, _, err := d.delegateSharesThisMachine(ctx, "br_missing", "br_caller"); err == nil {
		t.Error("a session the server does not know passed the check")
	}
}

func TestShareBrowserVideosIntoThisSessionPostsEachVideo(t *testing.T) {
	var shared []string
	server := fakeSessionServer(t, nil, nil, &shared)
	defer server.Close()
	videoDirectory := t.TempDir()
	for name, content := range map[string]string{"page@b.webm": "second", "page@a.webm": "first", "playwright-config.json": "{}"} {
		if err := os.WriteFile(filepath.Join(videoDirectory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LLM_BRIDGE_SESSION_FILES_URL", server.URL+"/files")
	t.Setenv("LLM_BRIDGE_SESSION_POSTING_TOKEN", "posting-token")
	if err := shareBrowserVideosIntoThisSession(videoDirectory); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"page@a.webm video/webm Bearer posting-token first",
		"page@b.webm video/webm Bearer posting-token second",
	}
	if strings.Join(shared, "\n") != strings.Join(want, "\n") {
		t.Errorf("shared:\n%s\nwant:\n%s", strings.Join(shared, "\n"), strings.Join(want, "\n"))
	}
}
