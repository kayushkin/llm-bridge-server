package tools

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Every case in browser_test.go asserts what came BACK from PinchTab. These
// assert what went OUT: which endpoint the request addressed, and with which
// parameters. A value the model chose reaches both a path segment and the query
// here, and concatenating it raw lets it move the request somewhere else.

// pathProbeIDs is the id table the fleet's path-segment pins use. Every entry
// carries a character that changes which endpoint the request addresses if it
// reaches the path unescaped.
//
// "well formed" and "space" are expected to agree before and after the repair:
// net/http encodes a space to %20 on its own, which is what url.PathEscape
// produces, so those two rows agree by accident rather than by care. They are
// kept so the table is the whole character class rather than only the subset
// that proves the point.
var pathProbeIDs = []struct {
	name string
	id   string
}{
	{"well formed", "inst-01HXYZ"},
	{"fragment", "inst#frag"},
	{"query", "inst?state=running"},
	{"extra segment", "a/b"},
	{"climbs out of the collection", "../instances"},
	{"space", "inst one"},
	{"already percent encoded", "inst%2Fb"},
}

// queryProbeFilters are snapshot filters carrying a character that is
// structural inside a query string. Every character here is LEGAL in a URL, so
// an unescaped request is sent successfully and mangled silently — which is the
// failure worth pinning.
//
// None of them contains a space on purpose: net/http refuses to build a request
// from a URL with a raw space, so a space-bearing probe reddens this pin by
// never reaching the server at all, and a reader learns nothing about a mangled
// value from that.
var queryProbeFilters = []struct {
	name   string
	filter string
}{
	{"well formed", "interactive"},
	{"injects a parameter", "interactive&all=1"},
	{"carries the pair separator", "kind=link"},
	{"already percent encoded", "interactive%26all=1"},
	{"fragment", "interactive#frag"},
	{"plus, which decodes to a space", "interactive+only"},
}

// recordingPinchtab starts a stub that keeps the raw request line of every
// request it answers, and returns the connection to it and the slice it
// appends to.
//
// It records r.RequestURI, NOT r.URL.Path or r.URL.Query(). Go's server has
// already decoded the request line by the time it fills URL, so an assertion
// over URL reads identically whether or not the client escaped anything — it
// cannot hold this property at all.
func recordingPinchtab(t *testing.T, instanceID string) (PinchtabConnection, *[]string) {
	t.Helper()
	var seen []string
	connection := pinchtabServing(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		// The tabs action reads GET /instances first and takes the id out of
		// the answer, so the id under test has to arrive from here.
		if r.URL.Path == "/instances" {
			body, err := json.Marshal([]map[string]string{{"id": instanceID}})
			if err != nil {
				t.Errorf("marshal instances: %v", err)
			}
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	return connection, &seen
}

func lastRequestLine(t *testing.T, seen *[]string) string {
	t.Helper()
	if len(*seen) == 0 {
		t.Fatal("the tool sent no request at all")
	}
	return (*seen)[len(*seen)-1]
}

// toolInput builds the one JSON blob a model hands the tool.
func toolInput(t *testing.T, fields map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	return string(raw)
}

// TestATabIDStaysOnePathSegment covers the close action, whose tab_id is a
// field on the tool's own input struct and so is filled by the model.
func TestATabIDStaysOnePathSegment(t *testing.T) {
	for _, probe := range pathProbeIDs {
		t.Run(probe.name, func(t *testing.T) {
			connection, seen := recordingPinchtab(t, "unused")
			runBrowser(t, connection, toolInput(t, map[string]string{"action": "close", "tab_id": probe.id}))

			want := "/tabs/" + url.PathEscape(probe.id) + "/close"
			if got := lastRequestLine(t, seen); got != want {
				t.Errorf("tab_id %q addressed %q, want %q", probe.id, got, want)
			}
		})
	}
}

// TestAnInstanceIDStaysOnePathSegment covers the tabs action. The id is not
// model-supplied — it comes back in PinchTab's own answer to GET /instances —
// but it is still a value this process did not choose, and escaping it costs
// nothing on a well-formed one.
func TestAnInstanceIDStaysOnePathSegment(t *testing.T) {
	for _, probe := range pathProbeIDs {
		t.Run(probe.name, func(t *testing.T) {
			connection, seen := recordingPinchtab(t, probe.id)
			runBrowser(t, connection, toolInput(t, map[string]string{"action": "tabs"}))

			want := "/instances/" + url.PathEscape(probe.id) + "/tabs"
			if got := lastRequestLine(t, seen); got != want {
				t.Errorf("instance id %q addressed %q, want %q", probe.id, got, want)
			}
		})
	}
}

// TestASnapshotFilterStaysOneQueryParameter is the query-position half. The
// property has two parts, and the second is the one an unescaped "&" breaks:
// the server must read back the value the caller passed, AND the parameter set
// must be exactly what the caller intended.
func TestASnapshotFilterStaysOneQueryParameter(t *testing.T) {
	for _, probe := range queryProbeFilters {
		t.Run(probe.name, func(t *testing.T) {
			connection, seen := recordingPinchtab(t, "unused")
			runBrowser(t, connection, toolInput(t, map[string]string{"action": "snapshot", "filter": probe.filter}))

			requestLine := lastRequestLine(t, seen)
			parsed, err := url.ParseRequestURI(requestLine)
			if err != nil {
				t.Fatalf("server could not parse the request line %q: %v", requestLine, err)
			}
			if parsed.Path != "/snapshot" {
				t.Errorf("filter %q moved the request off /snapshot to %q\n  request line: %s",
					probe.filter, parsed.Path, requestLine)
			}
			values := parsed.Query()
			if got := values.Get("filter"); got != probe.filter {
				t.Errorf("filter round-tripped as %q, want %q\n  request line: %s",
					got, probe.filter, requestLine)
			}
			for name := range values {
				if name != "filter" {
					t.Errorf("parameter %q appeared and the caller never set it — the filter carried a separator\n  request line: %s",
						name, requestLine)
				}
			}
		})
	}
}

// TestAnEmptyFilterSendsNoQueryAtAll pins the branch the repair had to leave
// alone. url.Values{"filter": {""}}.Encode() is "filter=", not "", so moving
// the escape outside the emptiness check would start sending PinchTab an empty
// filter it was never sent before.
func TestAnEmptyFilterSendsNoQueryAtAll(t *testing.T) {
	connection, seen := recordingPinchtab(t, "unused")
	runBrowser(t, connection, toolInput(t, map[string]string{"action": "snapshot"}))

	if got := lastRequestLine(t, seen); got != "/snapshot" {
		t.Errorf("snapshot with no filter addressed %q, want %q", got, "/snapshot")
	}
}

// TestTheRecorderCanSeeAnUnescapedValue is the rig guard. Every assertion above
// is read off r.RequestURI because that is the one field a client's escaping
// still shows up in. A recorder that could not tell the two apart would leave
// all four pins passing on unescaped code, and this file would be worthless.
//
// It drives the recorder with two hand-built request lines rather than through
// the tool, so it stays true whatever the tool does.
func TestTheRecorderCanSeeAnUnescapedValue(t *testing.T) {
	connection, seen := recordingPinchtab(t, "unused")
	base := connection.BaseURL
	if base == "" {
		t.Fatal("the recorder's connection has no BaseURL")
	}

	for _, line := range []string{"/tabs/a/b/close", "/tabs/a%2Fb/close"} {
		resp, err := http.Get(base + line)
		if err != nil {
			t.Fatalf("GET %s: %v", line, err)
		}
		resp.Body.Close()
	}
	if len(*seen) != 2 {
		t.Fatalf("recorder saw %d requests, want 2", len(*seen))
	}
	if (*seen)[0] == (*seen)[1] {
		t.Fatalf("the recorder read %q for both an unescaped and an escaped slash — "+
			"it cannot see the property every other pin in this file asserts", (*seen)[0])
	}
	if !strings.Contains((*seen)[1], "%2F") {
		t.Errorf("the escaped request line %q lost its %%2F", (*seen)[1])
	}
}
