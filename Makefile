# Common tasks. Run "make help" for a list.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build run test test-short test-race fuzz bench cover lint sdk sdk-test check clean docker

help: ## list the targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## build the server into bin/candledb
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/candledb ./cmd/candledb

run: build ## run a development server with open rules
	./bin/candledb serve -dev-open-rules

test: ## run all Go tests
	go test ./...

test-short: ## run Go tests, skipping the long randomised ones
	go test -short ./...

test-race: ## run all Go tests with the race detector
	go test -race ./...

fuzz: ## fuzz the rules parser for 30 seconds
	go test ./internal/rules -run '^$$' -fuzz FuzzParse -fuzztime 30s

bench: ## run the storage benchmarks
	go test ./internal/adapters/sqlite -run '^$$' -bench . -benchmem

cover: ## write coverage.out and print the total
	go test -short -coverpkg=./internal/...,./cmd/... -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

lint: ## check formatting and run go vet
	@test -z "$$(gofmt -l .)" || (echo "these files need gofmt:"; gofmt -l .; exit 1)
	go vet ./...

sdk: ## build the JavaScript client
	cd sdk/js && npm install --no-audit --no-fund && npm run build

sdk-test: build ## test the JavaScript client against the server
	cd sdk/js && npm install --no-audit --no-fund && npm test

check: lint test-race sdk-test ## everything CI runs

docker: ## build the container image
	docker build --build-arg VERSION=$(VERSION) -t candledb:$(VERSION) .

clean: ## remove build output
	rm -rf bin coverage.out sdk/js/dist
