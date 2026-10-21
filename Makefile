.PHONY: build test vet fmt check

build:
	go build -o sign-in-store ./cmd/sign-in-store

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test build
