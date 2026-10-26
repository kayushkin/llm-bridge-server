module github.com/kayushkin/llm-bridge-server

go 1.26.0

require (
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/creack/pty v1.1.24
	github.com/gorilla/websocket v1.5.3
	github.com/kayushkin/agent-store v0.0.0
	github.com/kayushkin/harness-store v0.0.0-00010101000000-000000000000
	github.com/kayushkin/hook-store v0.0.0-00010101000000-000000000000
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/kayushkin/log-store v0.0.0
	github.com/kayushkin/memory-store v0.0.0
	github.com/kayushkin/model-store v0.0.0
	github.com/kayushkin/snapshot-store v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.7.0
	github.com/oklog/ulid/v2 v2.1.1
	golang.org/x/oauth2 v0.36.0
	modernc.org/sqlite v1.48.2
	mvdan.cc/sh/v3 v3.13.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-sqlite3 v1.14.37 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/kayushkin/llm-bridge => ../llm-bridge

replace github.com/kayushkin/log-store => ../log-store

replace github.com/kayushkin/agent-store => ../agent-store

replace github.com/kayushkin/bus => ../bus

replace github.com/kayushkin/harness-store => ../harness-store

replace github.com/kayushkin/memory-store => ../memory-store

replace github.com/kayushkin/aiauth => ../aiauth

replace github.com/kayushkin/model-store => ../model-store

replace github.com/kayushkin/hook-store => ../hook-store

replace github.com/kayushkin/snapshot-store => ../snapshot-store
