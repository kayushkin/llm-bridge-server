// Package codex reads the live Codex subscription limits from the Codex CLI.
//
// It starts `codex app-server`, the JSON-RPC interface the Codex IDE extension
// uses, and asks it `account/rateLimits/read`. The CLI answers from OpenAI's
// servers with its own stored login and refreshes that login itself, so this
// package never reads ~/.codex/auth.json or holds a token.
//
// The answer covers every Codex client on the account — other machines, the IDE
// extension, cloud tasks — which the local session logs this package used to
// read could not see.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

// Window names exposed in ProviderLimits.Windows.
//
// The CLI names its windows primary and secondary; they are keyed by length:
// 300 minutes → five_hour, 10080 → weekly, anything else → "<n>m". A window that
// omits its length keys on its position, the only fact left that tells it apart.
// Windows is read by programs (the autoworker gates on Windows["weekly"]), so two
// windows must never share a key.
const (
	WindowFiveHour  = "five_hour"
	WindowWeekly    = "weekly"
	WindowPrimary   = "primary"
	WindowSecondary = "secondary"
)

// SourceAppServer is the ProviderLimits.Source this package reports.
const SourceAppServer = "app-server"

// rateLimitWindow is one window as `account/rateLimits/read` returns it.
type rateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"` // unix seconds
}

type rateLimitSnapshot struct {
	LimitID   string           `json:"limitId"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
	PlanType  *string          `json:"planType"`
}

type rateLimitsReadResult struct {
	RateLimits *rateLimitSnapshot `json:"rateLimits"`
}

// Reader asks a Codex CLI for the account's current limits.
type Reader struct {
	// Command is the resolved path of the codex executable.
	Command string
	// Timeout bounds one whole exchange, from starting the process to its exit.
	Timeout time.Duration
}

// New resolves command (a path, or a name looked up on PATH) and returns a Reader
// for it. It fails when the executable cannot be found.
func New(command string, timeout time.Duration) (*Reader, error) {
	if command == "" {
		return nil, errors.New("codex command is empty")
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("resolve codex command %q: %w", command, err)
	}
	return &Reader{Command: resolved, Timeout: timeout}, nil
}

// Read starts `codex app-server`, asks it for the account's rate limits and
// returns them with the raw JSON-RPC result.
func (r *Reader) Read(ctx context.Context) (*usagestore.ProviderLimits, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	// read-only sandbox and no approvals: this process only answers questions.
	cmd := exec.CommandContext(ctx, r.Command, "-s", "read-only", "-a", "never", "app-server")
	// The npm `codex` is a node wrapper around the native binary. Give the pair its
	// own process group so a timeout kills both, not just the wrapper.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("codex app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start codex app-server: %w", err)
	}

	raw, exchangeErr := exchange(stdin, stdout)
	// Closing stdin is how a client says it is done; the server exits on EOF.
	stdin.Close()
	waitErr := cmd.Wait()

	if exchangeErr != nil {
		if ctx.Err() != nil {
			exchangeErr = fmt.Errorf("%w (timed out after %s)", exchangeErr, r.Timeout)
		}
		return nil, nil, fmt.Errorf("codex app-server: %w%s", exchangeErr, stderrSuffix(&stderr))
	}
	if waitErr != nil {
		return nil, nil, fmt.Errorf("codex app-server exited badly after answering: %w%s", waitErr, stderrSuffix(&stderr))
	}

	var result rateLimitsReadResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, fmt.Errorf("decode account/rateLimits/read result: %w", err)
	}
	if result.RateLimits == nil {
		return nil, nil, fmt.Errorf("account/rateLimits/read returned no rateLimits: %s", raw)
	}
	out, err := normalize(result.RateLimits, time.Now())
	if err != nil {
		return nil, nil, err
	}
	return out, raw, nil
}

// rpcMessage covers the three shapes the server writes: a response (id with
// result or error), a notification (method, no id) and a server request.
type rpcMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const (
	initializeRequestID = 1
	rateLimitsRequestID = 2
)

// exchange runs the handshake and the one request over newline-delimited
// JSON-RPC, and returns the raw result of account/rateLimits/read.
func exchange(stdin io.Writer, stdout io.Reader) (json.RawMessage, error) {
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	send := func(message map[string]any) error {
		message["jsonrpc"] = "2.0"
		encoded, err := json.Marshal(message)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(encoded, '\n'))
		return err
	}

	if err := send(map[string]any{
		"id":     initializeRequestID,
		"method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{"name": "usage-store", "version": "1"}},
	}); err != nil {
		return nil, fmt.Errorf("send initialize: %w", err)
	}
	if _, err := awaitResponse(lines, initializeRequestID, "initialize"); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return nil, fmt.Errorf("send initialized: %w", err)
	}
	if err := send(map[string]any{"id": rateLimitsRequestID, "method": "account/rateLimits/read"}); err != nil {
		return nil, fmt.Errorf("send account/rateLimits/read: %w", err)
	}
	return awaitResponse(lines, rateLimitsRequestID, "account/rateLimits/read")
}

// awaitResponse reads until the response to id arrives, skipping notifications.
func awaitResponse(lines *bufio.Scanner, id int64, method string) (json.RawMessage, error) {
	for lines.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(lines.Bytes(), &message); err != nil {
			return nil, fmt.Errorf("undecodable line while waiting for %s: %w: %.200s", method, err, lines.Text())
		}
		if message.ID == nil || *message.ID != id || message.Method != "" {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf("%s failed: %d %s", method, message.Error.Code, message.Error.Message)
		}
		return message.Result, nil
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read while waiting for %s: %w", method, err)
	}
	return nil, fmt.Errorf("output ended before the %s response", method)
}

func stderrSuffix(stderr *bytes.Buffer) string {
	text := strings.TrimSpace(stderr.String())
	if text == "" {
		return ""
	}
	if len(text) > 2000 {
		text = text[len(text)-2000:]
	}
	return "; stderr: " + text
}

// normalize maps the CLI's primary/secondary windows onto stable keys.
//
// It fails rather than let one window overwrite another: two windows sharing a key
// means the snapshot contradicts itself, and keeping the last one would report one
// limit where the provider stated two.
func normalize(snap *rateLimitSnapshot, fetchedAt time.Time) (*usagestore.ProviderLimits, error) {
	out := &usagestore.ProviderLimits{
		Provider:   "codex",
		SnapshotAt: fetchedAt.Unix(),
		Source:     SourceAppServer,
		Windows:    map[string]*usagestore.LimitWindow{},
	}
	if snap.PlanType != nil {
		out.PlanType = *snap.PlanType
	}
	addWindow := func(w *rateLimitWindow, position string) error {
		if w == nil {
			return nil
		}
		key := keyForWindow(w.WindowDurationMins, position)
		if existing, taken := out.Windows[key]; taken {
			return fmt.Errorf(
				"codex limits at %s: %s window and an earlier window both key on %q (%.2f%% vs %.2f%%); refusing to drop one",
				fetchedAt.UTC().Format(time.RFC3339), position, key, existing.UsedPercent, w.UsedPercent)
		}
		out.Windows[key] = &usagestore.LimitWindow{
			UsedPercent:   w.UsedPercent,
			WindowMinutes: w.WindowDurationMins,
			ResetsAt:      w.ResetsAt,
		}
		return nil
	}
	if err := addWindow(snap.Primary, WindowPrimary); err != nil {
		return nil, err
	}
	if err := addWindow(snap.Secondary, WindowSecondary); err != nil {
		return nil, err
	}
	return out, nil
}

// keyForWindow names a window by its length, or by its position when the length
// is absent. position must be unique within one snapshot.
func keyForWindow(minutes *int64, position string) string {
	if minutes == nil {
		return position
	}
	switch *minutes {
	case 300:
		return WindowFiveHour
	case 10080:
		return WindowWeekly
	default:
		return fmt.Sprintf("%dm", *minutes)
	}
}
