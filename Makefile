.PHONY: build test lint fmt proto bench chaos spec up down clean

BIN_DIR := bin
VERSION ?= dev

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/raftlogd ./cmd/raftlogd
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/raftlogctl ./cmd/raftlogctl
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/raftlogconsole ./cmd/raftlogconsole
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/raftlogbench ./cmd/raftlogbench
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/orderservice ./cmd/orderservice

test:
	go test -race -count=1 -short ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       api/raftlog/v1/raftlog.proto api/raftlog/v1/raft.proto api/raftlog/v1/command.proto

bench: # micro-benchmarks; the cluster benchmark is bench/run-matrix.sh
	go test -run '^$$' -bench . -benchmem ./...

chaos:
	go test -race -count=1 -v ./test/chaos/... ./cmd/raftlogconsole/... ./cmd/raftlogbench/... ./cmd/orderservice/...

spec: # model-check the TLA+ specifications (docs/formal-spec); needs Java and TLA2TOOLS=/path/to/tla2tools.jar
	docs/formal-spec/check.sh

up:
	docker compose -f deploy/docker-compose.yml up --build

down:
	docker compose -f deploy/docker-compose.yml down -v

clean:
	rm -rf $(BIN_DIR) dist data
