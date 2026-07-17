BIN := ../bin/lark-watch

.PHONY: install build test

install: test build

build:
	go build -o $(BIN) .

test:
	go vet ./...
	go test ./...
