package httproutes

import "strings"

// PublicBaseURL converts a listen address (":8160", "0.0.0.0:8160", or a
// full URL) into an origin the harness can reach. For bare ports we resolve
// to localhost since the harness subprocess runs on the same host.
func PublicBaseURL(listenAddr string) string {
	if strings.HasPrefix(listenAddr, "http://") || strings.HasPrefix(listenAddr, "https://") {
		return strings.TrimRight(listenAddr, "/")
	}
	host := listenAddr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	return "http://" + host
}
