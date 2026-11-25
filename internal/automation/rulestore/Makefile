.PHONY: build test vet fmt check

build:
	go build -o rule-store ./cmd/rule-store

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test build
