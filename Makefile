export GOTOOLCHAIN := auto

.PHONY: test race vet lint build e2e

test:                       ## unit tests (juicefs is faked: no bucket, no database)
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./... && GOOS=linux go vet ./... && go vet -tags e2e ./test/e2e/

# core's pinned linter and its rules (core's make lint builds it into core/bin), from the workspace
GOLANGCI ?= ../core/bin/golangci-lint
LINTCONF ?= ../core/.golangci.yml

lint:                       ## this OS, Linux, and the e2e tests
	$(GOLANGCI) run --config $(LINTCONF) ./... && GOOS=linux $(GOLANGCI) run --config $(LINTCONF) ./... && GOOS=linux $(GOLANGCI) run --config $(LINTCONF) --build-tags e2e ./test/e2e/

build:                      ## the plugin for Linux workers and control planes
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/kevaso-provider-juicefs-linux-amd64 ./cmd/kevaso-provider-juicefs

e2e:                        ## on real Linodes: costs money; needs KEVASO_LINODE_TOKEN_FILE and the workspace (README)
	@test -n "$$KEVASO_LINODE_TOKEN_FILE" || { echo "set KEVASO_LINODE_TOKEN_FILE to a file holding a Linode API token"; exit 1; }
	go test -tags e2e -count=1 -v -timeout 100m -run TestJuiceFS ./test/e2e/
