package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The person a delegate works for cannot watch its browser: every browser on
// this host runs headless and is gone when the delegate stops. So when the
// bundle carries the Playwright MCP, bridge-agent has it record every page of
// the run into a folder of its own, and when the run ends shares each video
// into the chat of the session that ran bridge-agent. The calling agent does
// nothing for it, and the delegate does one thing: close the browser before it
// answers, because Playwright finishes a video only when its page closes. A
// recording cut off by the session stopping keeps only its first seconds
// (measured 2026-10-20: 1.7 s of a 5.8 s run).

// playwrightMCPPackage is the npm package an MCP server in a bundle runs when
// it is the Playwright MCP. The bundle's own name for the server is not used:
// a bundle may call it anything.
const playwrightMCPPackage = "@playwright/mcp"

// closeTheBrowserBeforeAnswering is added to the delegate's prompt when its
// browser is recorded.
const closeTheBrowserBeforeAnswering = "\n\n(From bridge-agent: this run's browser is recorded so the person can watch what you did. " +
	"Call browser_close before your final reply; a video is only saved when its page closes.)"

// browserVideoDirectory is a new, empty folder for one run's videos.
func browserVideoDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	directory := filepath.Join(home, ".local", "share", "bridge-agent", "browser-videos",
		time.Now().UTC().Format("20060102T150405Z")+"-"+hex.EncodeToString(random))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("create the browser video folder: %w", err)
	}
	return directory, nil
}

// recordPlaywrightBrowserVideos returns the bundle JSON with every Playwright
// MCP server told to record each page into videoDirectory, through a
// Playwright config file it writes there, and whether the bundle had one. A
// server that already names a --config is refused rather than overridden.
func recordPlaywrightBrowserVideos(bundleJSON string, videoDirectory string) (string, bool, error) {
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal([]byte(bundleJSON), &bundle); err != nil {
		return "", false, fmt.Errorf("read the MCP bundle: %w", err)
	}
	var servers map[string]map[string]any
	if err := json.Unmarshal(bundle["mcpServers"], &servers); err != nil {
		return "", false, fmt.Errorf("read the MCP bundle's mcpServers: %w", err)
	}
	configPath := filepath.Join(videoDirectory, "playwright-config.json")
	recording := false
	for name, server := range servers {
		rawArguments, _ := server["args"].([]any)
		runsPlaywright := false
		for _, argument := range rawArguments {
			text, _ := argument.(string)
			if text == "--config" || strings.HasPrefix(text, "--config=") {
				return "", false, fmt.Errorf("MCP server %q already names a Playwright --config; bridge-agent will not replace it to record the browser (pass -record-browser-video=false)", name)
			}
			if text == playwrightMCPPackage || strings.HasPrefix(text, playwrightMCPPackage+"@") {
				runsPlaywright = true
			}
		}
		if !runsPlaywright {
			continue
		}
		server["args"] = append(rawArguments, "--config", configPath)
		recording = true
	}
	if !recording {
		return bundleJSON, false, nil
	}
	playwrightConfig, err := json.Marshal(map[string]any{
		"browser": map[string]any{
			"contextOptions": map[string]any{
				"recordVideo": map[string]any{
					"dir":         videoDirectory,
					"size":        map[string]int{"width": 1280, "height": 720},
					"showActions": map[string]any{},
				},
			},
		},
	})
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(configPath, playwrightConfig, 0600); err != nil {
		return "", false, fmt.Errorf("write the Playwright config: %w", err)
	}
	encodedServers, err := json.Marshal(servers)
	if err != nil {
		return "", false, err
	}
	bundle["mcpServers"] = encodedServers
	rewritten, err := json.Marshal(bundle)
	if err != nil {
		return "", false, err
	}
	return string(rewritten), true, nil
}

// recordedBrowserVideos lists the videos a run left in its folder, oldest
// name first.
func recordedBrowserVideos(videoDirectory string) ([]string, error) {
	videos, err := filepath.Glob(filepath.Join(videoDirectory, "*.webm"))
	if err != nil {
		return nil, err
	}
	sort.Strings(videos)
	return videos, nil
}

// shareBrowserVideosIntoThisSession puts each recorded video into the chat of
// the bridge session bridge-agent runs in, as bridge-share-file does, with the
// token llm-bridge-server gives that session's processes. Outside a bridge
// session there is no chat, so it prints where the videos are instead.
func shareBrowserVideosIntoThisSession(videoDirectory string) error {
	videos, err := recordedBrowserVideos(videoDirectory)
	if err != nil {
		return err
	}
	if len(videos) == 0 {
		fmt.Fprintf(os.Stderr, "[bridge-agent] no browser video in %s: the delegate opened no page, or ran on another machine\n", videoDirectory)
		return nil
	}
	filesURL := os.Getenv("LLM_BRIDGE_SESSION_FILES_URL")
	postingToken := os.Getenv("LLM_BRIDGE_SESSION_POSTING_TOKEN")
	if filesURL == "" || postingToken == "" {
		for _, video := range videos {
			fmt.Fprintf(os.Stderr, "[bridge-agent] browser video: %s (not shared: this process is not in a bridge session)\n", video)
		}
		return nil
	}
	for _, video := range videos {
		fileID, err := shareFileIntoSession(filesURL, postingToken, video, "video/webm")
		if err != nil {
			return fmt.Errorf("share browser video %s: %w", video, err)
		}
		fmt.Fprintf(os.Stderr, "[bridge-agent] shared browser video %s into this session's chat as %s\n", video, fileID)
	}
	return nil
}

func shareFileIntoSession(filesURL, postingToken, path, mediaType string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequest(http.MethodPost, filesURL+"?filename="+url.QueryEscape(filepath.Base(path)), bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+postingToken)
	request.Header.Set("Content-Type", mediaType)
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return "", httpError(response)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	var shared struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(body, &shared); err != nil {
		return "", fmt.Errorf("read the share answer %q: %w", body, err)
	}
	return shared.FileID, nil
}

// delegateSharesThisMachine reports whether the delegate session runs on the
// machine of the session bridge-agent runs in, so that the delegate's browser
// writes its videos to a disk this process can read, and its Playwright config
// path names a file that exists where the browser starts. A Playwright MCP
// given a --config it cannot open exits at start (ENOENT, 2026-10-20), so on
// another machine recording would cost the delegate its browser. The answer
// comes from the two sessions' instances and the machine ids the instance
// catalog gives them; reason says why when the answer is no.
func (d *delegate) delegateSharesThisMachine(ctx context.Context, delegateSessionID, callingSessionID string) (bool, string, error) {
	if callingSessionID == "" {
		return false, "bridge-agent is not running in a bridge session, so it cannot tell which machine it is on", nil
	}
	delegateInstanceID, err := d.sessionInstanceID(ctx, delegateSessionID)
	if err != nil {
		return false, "", fmt.Errorf("read the delegate session's instance: %w", err)
	}
	callingInstanceID, err := d.sessionInstanceID(ctx, callingSessionID)
	if err != nil {
		return false, "", fmt.Errorf("read the calling session's instance: %w", err)
	}
	machineByInstance, err := d.machineIDByInstanceID(ctx)
	if err != nil {
		return false, "", fmt.Errorf("read the instance catalog: %w", err)
	}
	delegateMachineID, callingMachineID := machineByInstance[delegateInstanceID], machineByInstance[callingInstanceID]
	if delegateMachineID == "" || callingMachineID == "" {
		return false, fmt.Sprintf("the instance catalog does not give the machine of instance %s or %s", delegateInstanceID, callingInstanceID), nil
	}
	if delegateMachineID != callingMachineID {
		return false, fmt.Sprintf("the delegate's instance %s is on machine %s, and this session's %s is on %s", delegateInstanceID, delegateMachineID, callingInstanceID, callingMachineID), nil
	}
	return true, "", nil
}

func (d *delegate) sessionInstanceID(ctx context.Context, sessionID string) (string, error) {
	var session struct {
		InstanceID string `json:"instance_id"`
	}
	if err := d.getJSON(ctx, "/sessions/"+escapePathSegment(sessionID), &session); err != nil {
		return "", err
	}
	if session.InstanceID == "" {
		return "", fmt.Errorf("session %s has no instance_id", sessionID)
	}
	return session.InstanceID, nil
}

func (d *delegate) machineIDByInstanceID(ctx context.Context) (map[string]string, error) {
	var instances []struct {
		ID        string `json:"id"`
		MachineID string `json:"machine_id"`
	}
	if err := d.getJSON(ctx, "/instances", &instances); err != nil {
		return nil, err
	}
	machineByInstance := make(map[string]string, len(instances))
	for _, instance := range instances {
		machineByInstance[instance.ID] = instance.MachineID
	}
	return machineByInstance, nil
}

func (d *delegate) getJSON(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.server+path, nil)
	if err != nil {
		return err
	}
	response, err := d.do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return httpError(response)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

// withBrowserVideoRecording returns a run's video folder and the harness
// config whose bundle records into it, or an empty folder and the config
// unchanged when the bundle runs no Playwright MCP.
func withBrowserVideoRecording(harnessConfig map[string]any) (string, map[string]any, error) {
	bundleJSON, _ := harnessConfig["mcp_config"].(string)
	videoDirectory, err := browserVideoDirectory()
	if err != nil {
		return "", nil, err
	}
	rewritten, recording, err := recordPlaywrightBrowserVideos(bundleJSON, videoDirectory)
	if err != nil || !recording {
		if removeErr := os.RemoveAll(videoDirectory); removeErr != nil {
			return "", nil, fmt.Errorf("remove the unused browser video folder: %w", removeErr)
		}
		return "", harnessConfig, err
	}
	recorded := make(map[string]any, len(harnessConfig))
	for key, value := range harnessConfig {
		recorded[key] = value
	}
	recorded["mcp_config"] = rewritten
	return videoDirectory, recorded, nil
}
