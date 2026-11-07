module github.com/kayushkin/time-ledger-store

go 1.26.0

replace github.com/kayushkin/llm-bridge => ../llm-bridge

require (
	github.com/kayushkin/llm-bridge v0.0.0-00010101000000-000000000000
	github.com/mattn/go-sqlite3 v1.14.53
)

require github.com/teambition/rrule-go v1.8.2 // indirect
