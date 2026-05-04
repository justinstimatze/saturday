# Saturday — workspace Makefile.
#
# Each Go module produces a binary. `make build` writes them under ./bin/
# with a git-derived version baked in via -ldflags. `make install` writes
# them to $(go env GOPATH)/bin instead. Version handling per the project's
# Go-CLI convention: `var version = "dev"` overridden by `-X main.version`.

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
GOBIN   := $(shell go env GOPATH)/bin
BINDIR  := bin

.PHONY: all build install test vet lint tidy hooks ci version clean

all: build

build:
	@mkdir -p $(BINDIR)
	@cd saturday-mayor    && go build $(LDFLAGS) -o ../$(BINDIR)/saturday-mayor .
	@cd watcher           && go build $(LDFLAGS) -o ../$(BINDIR)/saturday-watcher .
	@cd saturday-hook     && go build $(LDFLAGS) -o ../$(BINDIR)/saturday-hook .
	@cd saturday-thinking && go build $(LDFLAGS) -o ../$(BINDIR)/saturday-thinking .
	@cd sync              && go build $(LDFLAGS) -o ../$(BINDIR)/saturday-sync .
	@echo "built $(VERSION) → $(BINDIR)/"

install:
	@cd saturday-mayor    && go build $(LDFLAGS) -o $(GOBIN)/saturday-mayor .
	@cd watcher           && go build $(LDFLAGS) -o $(GOBIN)/saturday-watcher .
	@cd saturday-hook     && go build $(LDFLAGS) -o $(GOBIN)/saturday-hook .
	@cd saturday-thinking && go build $(LDFLAGS) -o $(GOBIN)/saturday-thinking .
	@cd sync              && go build $(LDFLAGS) -o $(GOBIN)/saturday-sync .
	@echo "installed $(VERSION) → $(GOBIN)/"

test:
	@go test ./...

vet:
	@go vet ./...

lint:
	@golangci-lint run ./...

tidy:
	@for d in saturday-mayor watcher saturday-hook saturday-thinking sync eval eval/router llmcore; do \
		(cd $$d && go mod tidy); \
	done

hooks:
	@git config core.hooksPath .githooks
	@echo "git hooks → .githooks/ (pre-commit will mirror CI on every commit)"

# `make ci` runs the exact gates the GitHub Actions workflow does, locally,
# in the same per-module loop. Use it to debug CI failures without pushing.
ci:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt drift in:"; echo "$$out"; exit 1; fi
	@for d in eval eval/router llmcore saturday-hook saturday-mayor saturday-thinking sync watcher; do \
		(cd $$d && go vet ./...) || exit 1; \
	done
	@for d in eval eval/router llmcore saturday-hook saturday-mayor saturday-thinking sync watcher; do \
		(cd $$d && go test -race ./...) || exit 1; \
	done
	@$(MAKE) build >/dev/null
	@echo "ci ✓"

version:
	@echo $(VERSION)

clean:
	@rm -rf $(BINDIR)
