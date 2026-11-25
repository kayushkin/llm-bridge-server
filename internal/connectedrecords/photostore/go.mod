module github.com/kayushkin/photo-store

go 1.26.0

require (
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
)

require golang.org/x/image v0.47.0 // indirect

replace github.com/kayushkin/llm-bridge => ../llm-bridge
