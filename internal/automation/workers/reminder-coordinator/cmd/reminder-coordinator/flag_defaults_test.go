package main

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The defaults that ship, and the wiring that carries them to the Coordinator.
//
// Every one of these was unreachable until parseFlags and newCoordinator were
// lifted out of main(). The suite in main_test.go builds its Coordinator field
// by field, so it asserts values the tests themselves chose — the nine values
// the binary actually starts with had never been read by anything. The digest
// hour is the sharpest case: this is the only process on the box that reminds
// the user of anything, and a wrong hour does not fail, it nudges at a time
// nobody is reading.

// noEnvironment is a process started with none of these variables set, which
// is how the scheduler runs this binary.
var noEnvironment = servicesettings.MapEnvironment(map[string]string{})

// environmentWith answers only for the keys given to it, so a test states the
// environment it means rather than inheriting the one the test binary happens
// to have.
func environmentWith(values map[string]string) servicesettings.Environment {
	return servicesettings.MapEnvironment(values)
}

func TestTheDefaultsThatShipAreTheOnesThisBinaryStartsWith(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	cfg, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parsing an empty command line: %v", err)
	}

	// Spelled out one per line rather than as a struct literal comparison so a
	// failure names the flag that moved instead of printing two nine-field
	// structs and leaving the reader to diff them.
	for _, check := range []struct {
		flag string
		got  any
		want any
	}{
		{"-noteboard-url", cfg.noteboardURL, "http://localhost:8191"},
		{"-dash-url", cfg.dashURL, "http://localhost:8101"},
		{"-ask", cfg.askPath, "/home/someone/bin/ask"},
		{"-tag", cfg.scopeTag, "personal"},
		{"-digest-hour", cfg.digestHour, 7},
		{"-digest-minute", cfg.digestMinute, 30},
		{"-tz", cfg.timezone, "America/Los_Angeles"},
		{"-lookahead", cfg.lookahead, 7 * 24 * time.Hour},
		{"-dry-run", cfg.dryRun, false},
	} {
		if check.got != check.want {
			t.Errorf("%s default = %v, want %v", check.flag, check.got, check.want)
		}
	}
}

// The digest hour and minute are a pair of same-typed fields registered next to
// each other and read into a two-field struct. Nothing but a test distinguishes
// 7:30 from 30:07, and the swap compiles.
func TestTheDigestHourAndMinuteDoNotSwapOnTheWayToTheCoordinator(t *testing.T) {
	cfg, err := parseFlags([]string{"-digest-hour", "9", "-digest-minute", "5"}, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	coordinator := newCoordinator(cfg, time.UTC, time.Now())

	if coordinator.DigestAt.Hour != 9 {
		t.Errorf("digest hour = %d, want 9", coordinator.DigestAt.Hour)
	}
	if coordinator.DigestAt.Minute != 5 {
		t.Errorf("digest minute = %d, want 5", coordinator.DigestAt.Minute)
	}
}

// The two base URLs are the other same-typed pair. Swapped, the coordinator
// would ask noteboard for calendar events and dash for todos; both answer 404
// and the tick reports a failure that names neither service.
func TestEachBaseURLReachesTheClientItIsFor(t *testing.T) {
	cfg, err := parseFlags([]string{
		"-noteboard-url", "http://notes.example",
		"-dash-url", "http://dash.example",
		"-ask", "/opt/ask",
	}, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	coordinator := newCoordinator(cfg, time.UTC, time.Now())

	noteboard, ok := coordinator.noteboard.(*NoteboardClient)
	if !ok {
		t.Fatalf("noteboard is %T, want *NoteboardClient", coordinator.noteboard)
	}
	if noteboard.BaseURL != "http://notes.example" {
		t.Errorf("noteboard base URL = %q, want http://notes.example", noteboard.BaseURL)
	}

	calendar, ok := coordinator.calendar.(*GoogleCalendarMirror)
	if !ok {
		t.Fatalf("calendar is %T, want *GoogleCalendarMirror", coordinator.calendar)
	}
	if calendar.DashURL != "http://dash.example" {
		t.Errorf("dash URL = %q, want http://dash.example", calendar.DashURL)
	}

	herald, ok := coordinator.herald.(*Herald)
	if !ok {
		t.Fatalf("herald is %T, want *Herald", coordinator.herald)
	}
	if herald.AskPath != "/opt/ask" {
		t.Errorf("ask path = %q, want /opt/ask", herald.AskPath)
	}
}

// Both clients join their base URL to a path that already starts with "/", so a
// base URL entered with a trailing slash produces "//api/items". noteboard
// answers that with a 404 rather than a redirect, and the tick fails on every
// call rather than on a recognizable one.
func TestATrailingSlashOnEitherBaseURLIsTrimmedBeforeAnyRequestIsBuilt(t *testing.T) {
	cfg, err := parseFlags([]string{
		"-noteboard-url", "http://notes.example/",
		"-dash-url", "http://dash.example///",
	}, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	coordinator := newCoordinator(cfg, time.UTC, time.Now())

	if got := coordinator.noteboard.(*NoteboardClient).BaseURL; got != "http://notes.example" {
		t.Errorf("noteboard base URL = %q, want it trimmed to http://notes.example", got)
	}
	if got := coordinator.calendar.(*GoogleCalendarMirror).DashURL; got != "http://dash.example" {
		t.Errorf("dash URL = %q, want it trimmed to http://dash.example", got)
	}
}

func TestTheRemainingConfigFieldsReachTheCoordinator(t *testing.T) {
	cfg, err := parseFlags([]string{
		"-tag", "work",
		"-lookahead", "48h",
		"-dry-run",
	}, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	coordinator := newCoordinator(cfg, time.UTC, time.Now())

	if coordinator.ScopeTag != "work" {
		t.Errorf("scope tag = %q, want work", coordinator.ScopeTag)
	}
	if coordinator.Lookahead != 48*time.Hour {
		t.Errorf("lookahead = %v, want 48h", coordinator.Lookahead)
	}
	if !coordinator.DryRun {
		t.Error("dry run = false, want true — a -dry-run that does not reach the coordinator mutates noteboard and sends a herald session")
	}
}

// The scope tag is what keeps the coding backlog out of the user's digest. A
// default of "" would list every open todo on the box, which is the failure the
// -tag flag exists to prevent.
func TestTheDefaultScopeTagIsNotEmpty(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if cfg.scopeTag == "" {
		t.Fatal("default scope tag is empty — every open todo on the box would be coordinated into the personal digest")
	}
}

// main() calls time.LoadLocation on this string and log.Fatalf's if it does not
// load, so a typo in the default is not a wrong time — it is a binary that
// exits at startup and reminds the user of nothing at all.
func TestTheDefaultTimezoneNamesAZoneThatActuallyLoads(t *testing.T) {
	cfg, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := time.LoadLocation(cfg.timezone); err != nil {
		t.Fatalf("default -tz %q does not load: %v — the binary would log.Fatalf on startup", cfg.timezone, err)
	}
}

// Now is converted into the digest's zone, and every day boundary in the tick
// is derived from it. Left in UTC it would roll the day over at 5pm Pacific.
func TestTheCoordinatorsClockIsMovedIntoTheDigestZone(t *testing.T) {
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("loading the zone: %v", err)
	}
	cfg, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	// 03:00 UTC is still the previous day in Los Angeles, so a clock left in UTC
	// disagrees with this one about what "today" is.
	instant := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	coordinator := newCoordinator(cfg, losAngeles, instant)

	if coordinator.Now.Location() != losAngeles {
		t.Errorf("coordinator clock zone = %v, want America/Los_Angeles", coordinator.Now.Location())
	}
	if day := coordinator.Now.Day(); day != 14 {
		t.Errorf("coordinator clock day = %d, want 14 — 03:00 UTC is still the 14th in Los Angeles", day)
	}
}

// ------------------------------------------------- the environment defaults

func TestTheEnvironmentSuppliesTheThreeDefaultsThatReadIt(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"NOTEBOARD_URL": "http://notes.from-env",
		"DASH_URL":      "http://dash.from-env",
		"ASK_BIN":       "/env/ask",
		"REMINDER_TZ":   "Europe/Dublin",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if cfg.noteboardURL != "http://notes.from-env" {
		t.Errorf("NOTEBOARD_URL ignored: got %q", cfg.noteboardURL)
	}
	if cfg.dashURL != "http://dash.from-env" {
		t.Errorf("DASH_URL ignored: got %q", cfg.dashURL)
	}
	if cfg.askPath != "/env/ask" {
		t.Errorf("ASK_BIN ignored: got %q", cfg.askPath)
	}
	if cfg.timezone != "Europe/Dublin" {
		t.Errorf("REMINDER_TZ ignored: got %q", cfg.timezone)
	}
}

// The registry's rule is "set and non-empty wins". Inverted, a set variable would
// be discarded and an unset one would be preferred — which is invisible on a
// box where none of the three are set, and is exactly how this binary runs.
func TestAnEmptyEnvironmentVariableDoesNotBeatTheBuiltInDefault(t *testing.T) {
	cfg, err := parseFlags(nil, environmentWith(map[string]string{
		"NOTEBOARD_URL": "",
		"DASH_URL":      "",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if cfg.noteboardURL != "http://localhost:8191" {
		t.Errorf("an empty NOTEBOARD_URL took over: got %q", cfg.noteboardURL)
	}
	if cfg.dashURL != "http://localhost:8101" {
		t.Errorf("an empty DASH_URL took over: got %q", cfg.dashURL)
	}
}

func TestAnExplicitFlagBeatsTheEnvironment(t *testing.T) {
	cfg, err := parseFlags([]string{"-noteboard-url", "http://notes.from-flag"},
		environmentWith(map[string]string{"NOTEBOARD_URL": "http://notes.from-env"}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if cfg.noteboardURL != "http://notes.from-flag" {
		t.Errorf("noteboard URL = %q, want the flag to win over the environment", cfg.noteboardURL)
	}
}

// The -ask default is the only home-relative one. It is the path to the herald
// binary, so a wrong one means the single nudge per tick silently fails to
// launch while the digest note still looks healthy.
func TestTheHeraldPathDefaultsUnderTheHomeDirectory(t *testing.T) {
	t.Setenv("HOME", "/var/lib/somewhere-else")
	cfg, err := parseFlags(nil, noEnvironment)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	want := filepath.Join("/var/lib/somewhere-else", "bin", "ask")
	if cfg.askPath != want {
		t.Errorf("ask path = %q, want %q", cfg.askPath, want)
	}
}

// ------------------------------------------------------------- the seam itself

// ContinueOnError is the whole reason this is callable from a test. Under
// ExitOnError a bad argument takes the test binary down, and the failure reads
// as a crashed suite rather than as a rejected flag.
func TestABadArgumentIsReturnedRatherThanExitingTheProcess(t *testing.T) {
	if _, err := parseFlags([]string{"-digest-hour", "half-past"}, noEnvironment); err == nil {
		t.Fatal("an unparseable -digest-hour was accepted")
	}
	if _, err := parseFlags([]string{"-no-such-flag"}, noEnvironment); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

// main() maps this one error to exit code 0 and everything else to 2. If -h
// stopped being distinguishable, asking for help would report failure.
func TestAskingForHelpIsDistinguishableFromABadArgument(t *testing.T) {
	_, err := parseFlags([]string{"-h"}, noEnvironment)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h returned %v, want flag.ErrHelp — main() exits 2 rather than 0 without it", err)
	}
}

// A package-level flag.CommandLine would panic on the second registration. That
// would only show up once a second test called parseFlags, so it is pinned
// rather than left to the order tests happen to run in.
func TestParseFlagsCanBeCalledMoreThanOnce(t *testing.T) {
	for attempt := range 3 {
		if _, err := parseFlags(nil, noEnvironment); err != nil {
			t.Fatalf("call %d: %v", attempt+1, err)
		}
	}
}

// A misspelled REMINDER_TZ used to be ignored, and the digest went out on Los
// Angeles time. It now stops the binary, and main prints why.
func TestAMisspelledReminderVariableIsRefused(t *testing.T) {
	_, err := parseFlags(nil, environmentWith(map[string]string{"REMINDER_TIMEZONE": "Europe/Dublin"}))
	if !errors.Is(err, errSettingsRefused) {
		t.Fatalf("parseFlags = %v, want errSettingsRefused", err)
	}
}
