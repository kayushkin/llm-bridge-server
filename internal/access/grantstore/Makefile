.PHONY: build test vet fmt check

build:
	go build -o grant-store ./cmd/grant-store

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test build
