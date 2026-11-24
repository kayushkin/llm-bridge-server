module github.com/kayushkin/kanban-store

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
)

replace github.com/kayushkin/llm-bridge => ../llm-bridge
