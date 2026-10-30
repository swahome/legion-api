include $(wildcard .env)
export

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS    ?= linux
GOARCH  ?= amd64
SQLC    := go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0

.PHONY: build run test generate migrate user lint

build: ## single static binary at bin/legion-api (migrations embedded)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o bin/legion-api ./cmd/legion-api

run: ## run the API locally, reading .env
	go run ./cmd/legion-api serve

test: ## run all tests (needs TEST_DATABASE_URL)
	go test -race -count=1 ./...

generate: ## regenerate sqlc code after changing db/queries or migrations
	$(SQLC) generate

migrate: ## apply migrations to DATABASE_URL
	go run ./cmd/legion-api migrate

user: ## create a dev admin: make user PHONE=0700000001 PASSWORD=secret123
	go run ./cmd/legion-api create-user -phone "$(PHONE)" -password "$(PASSWORD)" -admin

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"
