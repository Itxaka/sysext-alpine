VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: build build-static test e2e clean lint

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sysext ./cmd/sysext

# Static binary for Alpine (musl-independent: pure Go, netgo not needed)
build-static:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS) -extldflags "-static"' -o bin/sysext ./cmd/sysext
	ln -sf sysext bin/confext

test:
	go test ./...

# Privileged end-to-end tests inside an Alpine container (real mounts).
e2e: build-static
	./test/e2e/run.sh

clean:
	rm -rf bin
