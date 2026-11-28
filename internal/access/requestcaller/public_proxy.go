package requestcaller

import "net/http"

// PublicProxyHeaders are set by the reverse proxy in front of this server on
// every request it forwards. A harness child, a sidecar or a runner reaching
// 127.0.0.1 directly or through an SSH tunnel sends none of them.
var PublicProxyHeaders = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded"}

// RequestCameThroughPublicProxy reports whether any header the public reverse
// proxy sets is present.
func RequestCameThroughPublicProxy(r *http.Request) bool {
	for _, name := range PublicProxyHeaders {
		if _, present := r.Header[http.CanonicalHeaderKey(name)]; present {
			return true
		}
	}
	return false
}
