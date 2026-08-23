package harness

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The runner asset URL is composed here and fetched by the runner, which is
// the same process that supplied os and arch in its Hello frame. These pins
// assert the composed URL survives a real round trip: what the receiving
// handler reads out of the query must be byte-for-byte what was put in.
//
// The probe deliberately contains no space. net/http refuses to parse a URL
// containing one, so a space fails the request outright rather than
// corrupting a parameter — a loud failure standing in for the silent one
// this file is about. That case is pinned separately, and named so, at the
// foot of the file.
const queryHostileProbe = "tok+en&injected=1&name=substituted"

// fetchQuery drives a real request through net/http and returns the query the
// server saw. Asserting on the server side rather than on the composed string
// is what makes these pins independent of how the URL was built.
func fetchQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()
	var seen url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
	}))
	defer srv.Close()

	target, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("composed URL does not parse: %v", err)
	}
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("test server URL does not parse: %v", err)
	}
	target.Scheme, target.Host = base.Scheme, base.Host

	resp, err := http.Get(target.String())
	if err != nil {
		t.Fatalf("request never reached the server: %v", err)
	}
	resp.Body.Close()
	return seen
}

func TestAnArchCarryingReservedCharactersStaysOneParameter(t *testing.T) {
	got := fetchQuery(t, assetURL("http://example.invalid", "inber-server", "linux", queryHostileProbe))

	if got.Get("arch") != queryHostileProbe {
		t.Errorf("arch was mangled in transit:\n got %q\nwant %q", got.Get("arch"), queryHostileProbe)
	}
	if got.Has("injected") {
		t.Errorf("arch injected a parameter the composer never wrote: injected=%q", got.Get("injected"))
	}
	if n := len(got); n != 3 {
		t.Errorf("want exactly name, os and arch; got %d parameters: %v", n, got)
	}
}

// The sharpest of the three. Go's url.Values.Get returns the FIRST value for a
// key, so an os injecting "&name=..." ahead of the real arch does not change
// name — but it does land ahead of arch, and the server then reads an arch the
// composer never chose. A wrong repair that escapes only some characters
// leaves exactly this.
func TestAnOSCannotDecideWhichArchTheServerReads(t *testing.T) {
	hostileOS := "linux&arch=substituted"
	got := fetchQuery(t, assetURL("http://example.invalid", "inber-server", hostileOS, "amd64"))

	if got.Get("os") != hostileOS {
		t.Errorf("os was mangled in transit:\n got %q\nwant %q", got.Get("os"), hostileOS)
	}
	if got.Get("arch") != "amd64" {
		t.Errorf("os overrode arch: server read arch=%q, composer wrote %q", got.Get("arch"), "amd64")
	}
}

func TestANameCarryingReservedCharactersStaysOneParameter(t *testing.T) {
	got := fetchQuery(t, assetURL("http://example.invalid", queryHostileProbe, "linux", "amd64"))

	if got.Get("name") != queryHostileProbe {
		t.Errorf("name was mangled in transit:\n got %q\nwant %q", got.Get("name"), queryHostileProbe)
	}
	if got.Get("os") != "linux" || got.Get("arch") != "amd64" {
		t.Errorf("name displaced its neighbours: os=%q arch=%q", got.Get("os"), got.Get("arch"))
	}
}

// The path must stay the path. url.Values on the query is not an excuse to
// stop caring where the "?" falls.
func TestTheAssetPathIsUnchangedByEscaping(t *testing.T) {
	u, err := url.Parse(assetURL("http://example.invalid/", "inber-server", "linux", "amd64"))
	if err != nil {
		t.Fatalf("composed URL does not parse: %v", err)
	}
	if u.Path != "/api/runner/binary" {
		t.Errorf("path is %q, want %q", u.Path, "/api/runner/binary")
	}
	if u.Host != "example.invalid" {
		t.Errorf("host is %q, want %q", u.Host, "example.invalid")
	}
}

// This is the case the probe above deliberately excludes, kept so it cannot
// stand in for the quiet one. A space makes net/http refuse the URL outright,
// so the failure is "the request was never sent" — loud, attributable, and
// nothing to do with a parameter being silently rewritten.
func TestASpaceInTheArchIsNotWhatThesePinsAreAbout(t *testing.T) {
	raw := assetURL("http://example.invalid", "inber-server", "linux", "amd 64")
	if _, err := http.NewRequest(http.MethodGet, raw, nil); err == nil {
		t.Skip("net/http now accepts a space in a URL; this pin's premise is gone")
	}
}
