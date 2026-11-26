# Common tasks. Run "make help" for a list.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build run test test-short test-race test-unit test-integration fuzz bench bench-smoke \
	cover cover-check lint tidy-check staticcheck vuln rules-check cross sdk sdk-test sdk-pack \
	check clean docker docker-smoke

# The lowest total statement coverage "make cover-check" accepts, in percent.
COVER_MIN ?= 85

# The platforms "make cross" compiles for.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

help: ## list the targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-17s %s\n", $$1, $$2}'

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

test-unit: ## run the Go tests of the packages, without the integration suite
	go test -race $$(go list ./... | grep -v /test/)

test-integration: ## run the tests that drive the whole server over HTTP
	go test -race -count=1 ./test/...

fuzz: ## fuzz the rules parser for 30 seconds
	go test ./internal/rules -run '^$$' -fuzz FuzzParse -fuzztime 30s

bench: ## run the storage benchmarks
	go test ./internal/adapters/sqlite -run '^$$' -bench . -benchmem

bench-smoke: ## run every benchmark once, to prove they still work
	go test ./... -run '^$$' -bench . -benchtime 1x

cover: ## write coverage.out and print the total
	go test -short -coverpkg=./internal/...,./cmd/... -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

cover-check: cover ## fail if total coverage is below COVER_MIN percent
	@go tool cover -func=coverage.out | awk -v min=$(COVER_MIN) '/^total:/ { sub("%", "", $$3); \
		if ($$3 + 0 < min) { printf "coverage %s%% is below the minimum of %s%%\n", $$3, min; exit 1 } \
		else printf "coverage %s%% meets the minimum of %s%%\n", $$3, min }'

lint: ## check formatting and run go vet
	@test -z "$$(gofmt -l .)" || (echo "these files need gofmt:"; gofmt -l .; exit 1)
	go vet ./...

tidy-check: ## fail if go.mod or go.sum are out of date
	go mod tidy -diff

staticcheck: ## run the staticcheck analyser
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln: ## look for known vulnerabilities in the code's dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

rules-check: build ## validate the example app's security rules
	./bin/candledb check-rules examples/todo/candledb.rules

cross: ## compile the server for every supported platform
	@for p in $(PLATFORMS); do \
		echo "build $$p"; \
		GOOS=$${p%/*} GOARCH=$${p#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/candledb || exit 1; \
	done

sdk: ## build the JavaScript client
	cd sdk/js && npm install --no-audit --no-fund && npm run build

sdk-test: build ## test the JavaScript client against the server
	cd sdk/js && npm install --no-audit --no-fund && npm test

sdk-pack: sdk ## check the contents of the npm package without publishing it
	cd sdk/js && node scripts/check-pack.mjs

check: lint tidy-check staticcheck vuln test-race cover-check bench-smoke rules-check cross sdk-test sdk-pack ## everything CI runs, except the container

docker: ## build the container image
	docker build --build-arg VERSION=$(VERSION) -t candledb:$(VERSION) .

docker-smoke: ## build the image, start it, and check that it serves
	./scripts/docker-smoke.sh

clean: ## remove build output
	rm -rf bin coverage.out sdk/js/dist
