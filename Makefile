VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
ARCHES := amd64 arm64 riscv64

.PHONY: build build-static build-all test e2e clean lint

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sysext ./cmd/sysext

# Static binary for Alpine (musl-independent: pure Go, netgo not needed)
build-static:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS) -extldflags "-static"' -o bin/sysext ./cmd/sysext
	ln -sf sysext bin/confext

# Cross-compile static binaries for all supported architectures.
build-all:
	for arch in $(ARCHES); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS) -extldflags "-static"' -o bin/sysext-linux-$$arch ./cmd/sysext || exit 1; \
	done

test:
	go test ./...

# Regenerate the example signed sysext (needs openssl + systemd-repart).
example:
	./examples/make-signed-sysext.sh

# Unit-test coverage: per-function summary + HTML report.
cover:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -n 1

# Privileged end-to-end tests inside an Alpine container (real mounts).
e2e: build-static
	./test/e2e/run.sh

# e2e with a coverage-instrumented binary (Go binary coverage): the container
# writes GOCOVERDIR data into .covdata/, converted to coverage-e2e.out.
# This measures the mount/loop/dm paths that unit tests cannot reach.
e2e-cover:
	CGO_ENABLED=0 go build -cover -covermode=atomic $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sysext ./cmd/sysext
	ln -sf sysext bin/confext
	rm -rf .covdata && mkdir -p .covdata
	COVDIR=$(CURDIR)/.covdata ./test/e2e/run.sh
	go tool covdata percent -i=.covdata
	go tool covdata textfmt -i=.covdata -o=coverage-e2e.out
	@go tool cover -func=coverage-e2e.out | tail -n 1

# Removes bin/ entirely, including cross-compiled bin/sysext-linux-* artifacts.
clean:
	rm -rf bin
