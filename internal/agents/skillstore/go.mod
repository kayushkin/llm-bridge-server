module github.com/kayushkin/skill-store

go 1.25.0

require (
	github.com/kayushkin/llm-bridge v0.0.0
	github.com/mattn/go-sqlite3 v1.14.37
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/kayushkin/llm-bridge => ../llm-bridge
