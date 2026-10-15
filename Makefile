.PHONY: build test lint

build:
	go build -o bin/luxmux ./cmd/luxmux

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
