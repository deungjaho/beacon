APP_NAME ?= beacon
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
LDFLAGS := -s -w

# 交叉编译：make build GOOS=linux GOARCH=amd64
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

.PHONY: all build install uninstall test vet fmt fmt-check clean

all: build

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/beacon

install: build
	@mkdir -p $(BINDIR)
	@rm -f $(BINDIR)/$(APP_NAME)
	install -m 0755 bin/$(APP_NAME) $(BINDIR)/$(APP_NAME)
	@echo "✓ $(APP_NAME) installed to $(BINDIR)/$(APP_NAME)"

uninstall:
	rm -f $(BINDIR)/$(APP_NAME)

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .
	goimports -w . 2>/dev/null || true

fmt-check:
	@diff=$$(gofmt -l .); if [ -n "$$diff" ]; then \
		echo "gofmt found unformatted files:"; echo "$$diff"; exit 1; fi

clean:
	rm -rf bin/
