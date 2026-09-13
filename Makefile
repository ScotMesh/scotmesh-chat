# Everything CI does, runnable locally. `make check` before every push.

MODULE   := github.com/ScotMesh/scotmesh-chat
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# CI builds with the toolchain go.mod names; locally we ask for the same one so
# lint and vuln results match (newer toolchains can break staticcheck's analysis).
export GOTOOLCHAIN ?= $(shell go mod edit -json | sed -n 's/.*"Toolchain": "\(.*\)".*/\1/p')

GOLANGCI := $(shell command -v golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)
LDFLAGS  := -s -w -buildid= -X main.version=$(VERSION)

.PHONY: check fmt lint vet test race cover fuzz vuln build third-party clean

check: vet lint race cover third-party vuln

fmt:
	gofmt -w $$(git ls-files '*.go' | grep -v '^third_party/')
	$(GOLANGCI) fmt ./...

vet:
	go vet ./...

lint:
	$(GOLANGCI) run ./...

test:
	go test -count=1 ./...

race:
	go test -race -shuffle=on -count=1 ./...

cover:
	go test -count=1 -coverprofile=cover.out ./...
	scripts/coverage-gate.sh cover.out

fuzz:
	scripts/fuzz.sh $(or $(FUZZTIME),30s)

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

third-party:
	cd third_party/reticulum-go && go test -race -count=1 ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/scotmesh-chat ./cmd/scotmesh-chat
	cd bin && sha256sum scotmesh-chat > scotmesh-chat.sha256
	@cat bin/scotmesh-chat.sha256

clean:
	rm -rf bin cover.out
