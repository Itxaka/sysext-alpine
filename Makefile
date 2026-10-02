VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
ARCHES := amd64 arm64 riscv64
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0

export CGO_ENABLED := 0

.PHONY: build build-static build-all test test-race lint vuln fuzz example cover e2e e2e-shared e2e-diff e2e-cover clean

# Static binary (pure Go, CGO disabled) plus the confext symlink.
build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sysext ./cmd/sysext
	ln -sf sysext bin/confext

build-static: build

build-all:
	for arch in $(ARCHES); do \
		GOOS=linux GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sysext-linux-$$arch ./cmd/sysext || exit 1; \
	done

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

FUZZTIME ?= 30s
fuzz:
	@for pkg in $$(go list ./...); do \
		for f in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
			echo "== $$pkg $$f"; \
			go test $$pkg -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) || exit 1; \
		done; \
	done

# Regenerate the example signed sysext (needs openssl + systemd-repart).
example:
	./examples/make-signed-sysext.sh

# Unit-test coverage: per-function summary + HTML report.
cover:
	go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -n 1

# Privileged end-to-end suites, each in a fresh container (real mounts, loop
# and dm devices). SUITES selects suites (e.g. SUITES="cli differential");
# test/e2e/run.sh documents E2E_PROPAGATION, E2E_STRICT and the other knobs.
SUITES ?=
e2e: build-static
	./test/e2e/run.sh $(SUITES)

# Every suite with / private and again with / rshared (as on hosts running
# k3s, kubelet or containerd).
e2e-shared: build-static
	E2E_PROPAGATION="private shared" ./test/e2e/run.sh $(SUITES)

# The differential suite against systemd-sysext 262 (archlinux:latest).
e2e-diff: build-static
	./test/e2e/run.sh differential

# e2e with a coverage-instrumented binary (Go binary coverage): the containers
# write GOCOVERDIR data into .covdata/, converted to coverage-e2e.out. This
# measures the mount/loop/dm paths that unit tests cannot reach. The binary
# stays static (CGO_ENABLED=0), so the archlinux suite contributes too. The
# coverage report is written even when a suite fails.
e2e-cover:
	go build -cover -covermode=atomic $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sysext ./cmd/sysext
	ln -sf sysext bin/confext
	rm -rf .covdata && mkdir -p .covdata
	COVDIR=$(CURDIR)/.covdata ./test/e2e/run.sh $(SUITES); rc=$$?; \
	go tool covdata percent -i=.covdata && \
	go tool covdata textfmt -i=.covdata -o=coverage-e2e.out && \
	go tool cover -func=coverage-e2e.out | tail -n 1; \
	exit $$rc

# Removes bin/ entirely, including cross-compiled bin/sysext-linux-* artifacts.
clean:
	rm -rf bin
