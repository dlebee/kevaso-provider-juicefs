export GOTOOLCHAIN := auto

.PHONY: test race vet build

test:                       ## unit tests (juicefs is faked: no bucket, no database)
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./... && GOOS=linux go vet ./...

build:                      ## the plugin for Linux workers and control planes
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/kevaso-provider-juicefs-linux-amd64 ./cmd/kevaso-provider-juicefs
