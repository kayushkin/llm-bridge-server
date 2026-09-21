module github.com/slava/scheduler

go 1.25.0

require (
	github.com/kayushkin/bus v0.0.0
	github.com/robfig/cron/v3 v3.0.1
	modernc.org/sqlite v1.50.0
)

require github.com/teambition/rrule-go v1.8.2 // indirect

replace (
	github.com/kayushkin/bus => ../bus
	github.com/kayushkin/llm-bridge => ../llm-bridge
	github.com/kayushkin/logstack => ../logstack
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/kayushkin/noteboard v0.0.0
	github.com/klauspost/compress v1.18.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/nats-io/nats.go v1.49.0 // indirect
	github.com/nats-io/nkeys v0.4.12 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.46.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.72.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/kayushkin/noteboard => ../noteboard
