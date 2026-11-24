module github.com/kayushkin/calendar-store

go 1.26.0

require (
	github.com/emersion/go-ical v0.0.0-20250609112844-439c63cef608
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
)

require github.com/teambition/rrule-go v1.8.2 // indirect

replace github.com/kayushkin/llm-bridge => ../llm-bridge
