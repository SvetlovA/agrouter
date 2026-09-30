# RACE is empty locally (no cgo on the dev machine); CI passes RACE=-race
RACE ?=

all: test build

build:
	go build -o .bin/ ./cmd/agrouter

test:
	go clean -testcache
	go test $(RACE) -coverprofile=coverage.out ./...
	grep -v "_mock.go" coverage.out | grep -v mocks > coverage_no_mocks.out
	go tool cover -func=coverage_no_mocks.out
	rm coverage.out coverage_no_mocks.out

lint:
	golangci-lint run --max-issues-per-linter=0 --max-same-issues=0

fmt:
	gofmt -s -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")
	goimports -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")

generate:
	go generate ./...

# placeholder: runs the build-tagged routing evaluation against the real Jev API (needs TYPESAFE_API_KEY), never in CI
eval-routing:
	@echo "eval-routing: not implemented yet"

.PHONY: all build test lint fmt generate eval-routing
