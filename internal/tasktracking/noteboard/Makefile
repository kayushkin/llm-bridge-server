# noteboard builds with the default Go toolchain and no cgo: the SQLite driver
# is modernc.org/sqlite, which is pure Go and ships the FTS5 module that
# internal/db.migrate() needs.
#
# It did not always. Until 2026-07-13 the driver was mattn/go-sqlite3 and this
# file had to export CGO_CFLAGS=-DSQLITE_ENABLE_FTS5 to get an FTS5-capable
# build — so anyone who ran a plain `go build` instead got a binary that
# compiled green and then died at boot with "no such module: fts5". Pinning
# CGO_ENABLED=0 is deliberate: a change that reintroduces a cgo SQLite driver
# now fails here, at build time, instead of at boot.
export CGO_ENABLED = 0

.PHONY: build test vet run clean

build:
	go build -o bin/noteboard ./cmd/noteboard/

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/noteboard

clean:
	rm -rf bin/
