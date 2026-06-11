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

# Privileged end-to-end tests inside an Alpine container (real mounts).
e2e: build-static
	./test/e2e/run.sh

# Removes bin/ entirely, including cross-compiled bin/sysext-linux-* artifacts.
clean:
	rm -rf bin
