.PHONY: build test lint fmt proto bench chaos up down clean

BIN_DIR := bin
VERSION ?= dev

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/raftlogd ./cmd/raftlogd
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/raftlogctl ./cmd/raftlogctl

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       api/raftlog/v1/raftlog.proto api/raftlog/v1/raft.proto api/raftlog/v1/command.proto

bench:
	go test -run '^$$' -bench . -benchmem ./...

chaos:
	go test -race -count=1 -tags=chaos ./test/chaos/...

up:
	docker compose -f deploy/docker-compose.yml up --build

down:
	docker compose -f deploy/docker-compose.yml down -v

clean:
	rm -rf $(BIN_DIR) dist data
