VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test run docker up down install

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/tasker ./cmd/tasker

test:
	go test ./...

run: build
	./bin/tasker serve --db ./tasker.db

install: build
	install -m 0755 bin/tasker $(HOME)/.local/bin/tasker

docker:
	docker build --build-arg VERSION=$(VERSION) -t tasker:latest .

up:
	docker compose up -d --build

down:
	docker compose down
