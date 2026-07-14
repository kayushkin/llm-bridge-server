// bridge-agent delegates a single prompt to a fresh llm-bridge session that
// runs with an explicitly chosen MCP bundle, waits for the turn to finish, and
// prints the agent's final text on stdout.
//
// It exists because MCP servers are a per-process cost. Claude Code spawns
// every configured stdio MCP server at session start whether or not the
// session ever calls one, and the browser MCPs (playwright, chrome-devtools)
// cost ~890MB per session. Sessions therefore run with NO MCP servers by
// default; a session that genuinely needs one shells out to this CLI, which
// spawns a separate short-lived session carrying only the requested bundle.
//
// A Claude Code Task() subagent cannot do this job: it runs in-process inside
// its parent and shares the parent's MCP connections, so it has no process of
// its own to attach a different MCP config to. The delegate has to be a real
// bridge session, which is what this CLI creates.
//
// The bundle is a plain Claude Code MCP config file under ~/.claude/mcp/.
// It reaches the harness through CreateSessionRequest.HarnessConfig, which
// llm-bridge-server merges verbatim into the harness start params; the
// claudecode harness turns mcp_config/strict_mcp_config into --mcp-config and
// --strict-mcp-config. The server never has to understand the bundle.
//
//	bridge-agent --mcp browser "open dash.kayushkin.com and screenshot the board"
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const (
	defaultServer  = "http://localhost:8160"
	defaultHarness = "claude_code"
	defaultTimeout = 15 * time.Minute
)

// bundleDir holds Claude Code MCP config files, one per bundle. The "none"
// bundle is an empty mcpServers map: combined with strict_mcp_config it is how
// a session is spawned with no MCP servers at all.
func bundleDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot resolve home directory: %v", err)
	}
	return filepath.Join(home, ".claude", "mcp")
}

func main() {
	var (
		bundle   = flag.String("mcp", "none", "MCP bundle to load — a config file name under ~/.claude/mcp (e.g. browser). \"none\" spawns with no MCP servers.")
		server   = flag.String("server", envOr("LLMBRIDGE_SERVER", defaultServer), "llm-bridge-server base URL")
		harness  = flag.String("harness", defaultHarness, "harness to run the delegate on")
		instance = flag.String("instance", os.Getenv("LLMBRIDGE_INSTANCE"), "harness instance id (defaults to the server's default instance)")
		purpose  = flag.String("purpose", "delegate", "session purpose recorded on the session row")
		timeout  = flag.Duration("timeout", defaultTimeout, "give up if the turn has not finished within this duration")
		rawJSON  = flag.Bool("json", false, "print the full result event as JSON instead of just its text")
		keep     = flag.Bool("keep", false, "leave the delegate session running instead of stopping it after the result")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: bridge-agent [flags] <prompt>\n\n"+
			"Runs <prompt> in a fresh llm-bridge session loaded with the named MCP bundle,\n"+
			"prints the final text, and stops the session. Reads the prompt from stdin if\n"+
			"no positional argument is given.\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if prompt == "" {
		piped, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal("read prompt from stdin: %v", err)
		}
		prompt = strings.TrimSpace(string(piped))
	}
	if prompt == "" {
		flag.Usage()
		os.Exit(2)
	}

	harnessConfig, err := loadBundle(*bundle)
	if err != nil {
		fatal("%v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	client := &http.Client{}
	agent := &delegate{client: client, server: strings.TrimRight(*server, "/")}

	sessionID, err := agent.createSession(ctx, *harness, *instance, *purpose, prompt, harnessConfig)
	if err != nil {
		fatal("create delegate session: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[bridge-agent] session %s (mcp=%s)\n", sessionID, *bundle)

	// Subscribe before sending so the result cannot land in the gap between
	// the send returning and the stream opening.
	events, closeStream, err := agent.subscribe(ctx, sessionID)
	if err != nil {
		agent.stop(sessionID)
		fatal("subscribe to session events: %v", err)
	}
	defer closeStream()

	if err := agent.send(ctx, sessionID, prompt); err != nil {
		agent.stop(sessionID)
		fatal("send prompt: %v", err)
	}

	result, err := waitForResult(ctx, events)
	if !*keep {
		agent.stop(sessionID)
	}
	if err != nil {
		fatal("%v", err)
	}

	if *rawJSON {
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Println(result.Text)
	}
	if result.IsError {
		os.Exit(1)
	}
}

// loadBundle returns the HarnessConfig that pins the delegate to exactly the
// MCP servers in the named bundle. strict_mcp_config makes the harness ignore
// every other MCP configuration source (user scope, project scope), so the
// bundle is the whole truth about what the delegate can reach.
func loadBundle(name string) (map[string]any, error) {
	path := filepath.Join(bundleDir(), name+".json")
	if _, err := os.Stat(path); err != nil {
		available, _ := filepath.Glob(filepath.Join(bundleDir(), "*.json"))
		for i, a := range available {
			available[i] = strings.TrimSuffix(filepath.Base(a), ".json")
		}
		return nil, fmt.Errorf("no MCP bundle %q at %s (available: %s)",
			name, path, strings.Join(available, ", "))
	}
	return map[string]any{
		"mcp_config":        path,
		"strict_mcp_config": true,
	}, nil
}

type delegate struct {
	client *http.Client
	server string
}

func (d *delegate) createSession(ctx context.Context, harness, instance, purpose, prompt string, harnessConfig map[string]any) (string, error) {
	cfg, err := json.Marshal(harnessConfig)
	if err != nil {
		return "", fmt.Errorf("marshal harness config: %w", err)
	}
	body, err := json.Marshal(msg.CreateSessionRequest{
		Harness:       msg.Harness(harness),
		InstanceID:    instance,
		DisplayName:   displayName(prompt),
		Type:          msg.SessionTypeAutonomous,
		Purpose:       purpose,
		Origin:        "bridge-agent",
		AutoStart:     true,
		HarnessConfig: cfg,
	})
	if err != nil {
		return "", err
	}

	resp, err := d.post(ctx, "/sessions", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", httpError(resp)
	}

	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	if created.SessionID == "" {
		return "", fmt.Errorf("server returned no session_id")
	}
	return created.SessionID, nil
}

func (d *delegate) send(ctx context.Context, sessionID, prompt string) error {
	body, err := json.Marshal(msg.SendMessageRequest{Message: prompt})
	if err != nil {
		return err
	}
	resp, err := d.post(ctx, "/sessions/"+sessionID+"/send", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpError(resp)
	}
	return nil
}

// stop tears the delegate down. llm-bridge-server also reaps delegate sessions
// on their first result, but this makes the teardown synchronous with the CLI
// exiting so the MCP servers are gone by the time the caller reads stdout.
func (d *delegate) stop(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := d.post(ctx, "/sessions/"+sessionID+"/stop", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[bridge-agent] warning: stop session %s: %v\n", sessionID, err)
		return
	}
	resp.Body.Close()
}

// subscribe opens the SSE event stream and decodes each frame into a msg.Event.
func (d *delegate) subscribe(ctx context.Context, sessionID string) (<-chan msg.Event, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.server+"/sessions/"+sessionID+"/events", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, nil, httpError(resp)
	}

	events := make(chan msg.Event)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			payload, ok := strings.CutPrefix(scanner.Text(), "data:")
			if !ok {
				continue
			}
			var ev msg.Event
			if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev); err != nil {
				continue
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, func() { resp.Body.Close() }, nil
}

// waitForResult consumes the stream until the turn produces its result. An
// error event or the session dropping into a terminal state ends the wait too,
// so a delegate that dies never hangs the caller until the timeout.
func waitForResult(ctx context.Context, events <-chan msg.Event) (*msg.ResultEvent, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for the delegate's result: %w", ctx.Err())
		case ev, open := <-events:
			if !open {
				return nil, fmt.Errorf("event stream closed before the delegate produced a result")
			}
			switch ev.Type {
			case msg.EventResult:
				if ev.Result == nil {
					return nil, fmt.Errorf("server sent a result event with no result payload")
				}
				return ev.Result, nil
			case msg.EventError:
				return nil, fmt.Errorf("delegate failed: %s", errorText(ev))
			}
		}
	}
}

func errorText(ev msg.Event) string {
	if ev.Error != nil && ev.Error.Message != "" {
		return ev.Error.Message
	}
	return "harness reported an error with no message"
}

func displayName(prompt string) string {
	const limit = 80
	name := strings.Join(strings.Fields(prompt), " ")
	if len(name) > limit {
		name = name[:limit] + "…"
	}
	return "delegate: " + name
}

func (d *delegate) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.client.Do(req)
}

func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bridge-agent: "+format+"\n", args...)
	os.Exit(1)
}
