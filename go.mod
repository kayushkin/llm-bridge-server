module github.com/kayushkin/llm-bridge-server

go 1.26.0

require (
	github.com/anthropics/anthropic-sdk-go v1.35.0
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/creack/pty v1.1.24
	github.com/emersion/go-ical v0.0.0-20250609112844-439c63cef608
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgx/v5 v5.13.0
	github.com/kayushkin/bus v0.0.0
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
	github.com/modelcontextprotocol/go-sdk v1.7.0
	github.com/nats-io/nats.go v1.49.0
	github.com/oklog/ulid/v2 v2.1.1
	github.com/robfig/cron/v3 v3.0.1
	github.com/spf13/cobra v1.10.2
	github.com/teambition/rrule-go v1.8.2
	golang.org/x/net v0.47.0
	golang.org/x/oauth2 v0.36.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.41.0
	golang.org/x/time v0.15.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.48.2
	mvdan.cc/sh/v3 v3.13.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/klauspost/compress v1.18.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/nats-io/nkeys v0.4.12 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/crypto v0.46.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/kayushkin/llm-bridge => ../llm-bridge

replace github.com/kayushkin/bus => ../bus
