BIN := ../bin/lark-watch
GOFLAGS := $(if $(RACE),-race)

.PHONY: install build test lint

install: lint test build

build:
	go build $(GOFLAGS) -o $(BIN) .

lint:
	golangci-lint run

test:
	go vet ./...
	go test -race ./...
