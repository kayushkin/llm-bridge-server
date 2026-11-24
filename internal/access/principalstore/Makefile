# The sqlite_fts5 tag is not optional and not a preference.
#
# schema.sql creates an FTS5 virtual table, and mattn/go-sqlite3 compiles FTS5
# into its bundled SQLite only when this build tag is set. A binary built without
# it compiles cleanly, links cleanly, and then dies at boot with
# "no such module: fts5". Every target here carries the tag so nobody has to
# remember it, and Open() names the flag if a binary built elsewhere shows up.
GO_TAGS := sqlite_fts5

.PHONY: build test vet fmt check

build:
	go build -tags $(GO_TAGS) -o principal-store ./cmd/principal-store

test:
	go test -tags $(GO_TAGS) ./...

vet:
	go vet -tags $(GO_TAGS) ./...

fmt:
	gofmt -w .

check: fmt vet test build
