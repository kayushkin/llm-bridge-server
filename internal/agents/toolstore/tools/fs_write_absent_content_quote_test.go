package tools

import "encoding/json"

// quote renders a path as a JSON string literal so a temp dir containing a
// backslash or a quote cannot silently malform the tool input under test.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
