// Package serversentevents holds what every server-sent event stream of this
// server shares.
package serversentevents

import "time"

// SseKeepaliveInterval is how often an otherwise-silent SSE stream emits a
// comment frame. It must stay comfortably below the idle read timeout of any
// intermediary in front of the server — nginx's proxy_read_timeout defaults to
// 60s, which is the shortest one this server is actually deployed behind.
const SseKeepaliveInterval = 25 * time.Second
