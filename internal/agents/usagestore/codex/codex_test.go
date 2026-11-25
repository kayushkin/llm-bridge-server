package codex

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

// A trimmed copy of a real `account/rateLimits/read` exchange, measured 2026-09-16
// against codex-cli 0.147.0, with a notification between the two responses.
const recordedServerOutput = `{"id":1,"result":{"userAgent":"usage-store/0.147.0","codexHome":"/home/x/.codex","platformFamily":"unix","platformOs":"linux"}}
{"method":"remoteControl/status/changed","params":{"status":"disabled"}}
{"id":2,"result":{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":93,"windowDurationMins":300,"resetsAt":1789595486},"secondary":{"usedPercent":30,"windowDurationMins":10080,"resetsAt":1790014681},"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"planType":"plus"}}}
`

func TestExchangeSkipsNotificationsAndReturnsTheRateLimitsResult(t *testing.T) {
	var sent bytes.Buffer
	raw, err := exchange(&sent, strings.NewReader(recordedServerOutput))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	for _, method := range []string{`"initialize"`, `"initialized"`, `"account/rateLimits/read"`} {
		if !strings.Contains(sent.String(), method) {
			t.Errorf("client never sent %s; sent:\n%s", method, sent.String())
		}
	}
	if !strings.Contains(string(raw), `"usedPercent":93`) {
		t.Errorf("raw result is not the rate-limits response: %s", raw)
	}
}

func TestExchangeSurfacesARPCError(t *testing.T) {
	output := `{"id":1,"result":{}}
{"id":2,"error":{"code":-32600,"message":"not logged in"}}
`
	_, err := exchange(io.Discard, strings.NewReader(output))
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v, want the server's error message", err)
	}
}

func TestExchangeFailsWhenOutputEndsEarly(t *testing.T) {
	_, err := exchange(io.Discard, strings.NewReader(`{"id":1,"result":{}}`+"\n"))
	if err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatalf("err = %v, want an early-end error", err)
	}
}

func TestNormalizeKeysWindowsByLength(t *testing.T) {
	plan := "plus"
	out, err := normalize(&rateLimitSnapshot{
		Primary:   &rateLimitWindow{UsedPercent: 93, WindowDurationMins: i64(300), ResetsAt: i64(1789595486)},
		Secondary: &rateLimitWindow{UsedPercent: 30, WindowDurationMins: i64(10080), ResetsAt: i64(1790014681)},
		PlanType:  &plan,
	}, time.Unix(1789593820, 0))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Source != SourceAppServer || out.PlanType != "plus" || out.SnapshotAt != 1789593820 {
		t.Errorf("header = %+v", out)
	}
	if w := out.Windows[WindowFiveHour]; w == nil || w.UsedPercent != 93 || *w.ResetsAt != 1789595486 {
		t.Errorf("five_hour = %+v", w)
	}
	if w := out.Windows[WindowWeekly]; w == nil || w.UsedPercent != 30 {
		t.Errorf("weekly = %+v", w)
	}
}

// Two windows with no length must not collapse onto one key.
func TestNormalizeKeepsBothWindowsWhenNeitherStatesItsLength(t *testing.T) {
	out, err := normalize(&rateLimitSnapshot{
		Primary:   &rateLimitWindow{UsedPercent: 11},
		Secondary: &rateLimitWindow{UsedPercent: 22},
	}, time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if out.Windows[WindowPrimary].UsedPercent != 11 || out.Windows[WindowSecondary].UsedPercent != 22 {
		t.Errorf("windows = %+v", out.Windows)
	}
}

func TestNormalizeRefusesTwoWindowsOfTheSameLength(t *testing.T) {
	_, err := normalize(&rateLimitSnapshot{
		Primary:   &rateLimitWindow{UsedPercent: 1, WindowDurationMins: i64(300)},
		Secondary: &rateLimitWindow{UsedPercent: 2, WindowDurationMins: i64(300)},
	}, time.Unix(1000, 0))
	if err == nil {
		t.Fatal("normalize kept one of two five-hour windows silently")
	}
}

func TestNewFailsForAMissingCommand(t *testing.T) {
	if _, err := New("codex-command-that-does-not-exist", time.Second); err == nil {
		t.Fatal("New accepted a command that is not on PATH")
	}
}
