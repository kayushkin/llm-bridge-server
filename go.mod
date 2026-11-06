module github.com/kayushkin/possession-store

go 1.26.0

require (
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/kayushkin/photo-store v0.0.0-00010101000000-000000000000
	github.com/mattn/go-sqlite3 v1.14.37
	golang.org/x/image v0.47.0
)

replace (
	github.com/kayushkin/llm-bridge => ../llm-bridge
	github.com/kayushkin/photo-store => ../photo-store
)
