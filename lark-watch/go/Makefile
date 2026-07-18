BIN := ../bin/lark-watch
GOFLAGS := $(if $(RACE),-race)

.PHONY: install build test

install: test build

build:
	go build $(GOFLAGS) -o $(BIN) .

test:
	go vet ./...
	go test -race ./...
