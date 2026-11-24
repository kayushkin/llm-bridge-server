.PHONY: build test run clean install

build:
	go build -o bin/kanban-store ./cmd/kanban-store/

test:
	go test ./...

run: build
	./bin/kanban-store

install: build
	install -m 0755 bin/kanban-store $(HOME)/bin/kanban-store

clean:
	rm -rf bin/
