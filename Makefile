APP_NAME ?= beacon
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: all build install test clean

all: build

build:
	go build -ldflags="-s -w" -o bin/$(APP_NAME) ./cmd/beacon

install:
	@mkdir -p $(BINDIR)
	@rm -f $(BINDIR)/$(APP_NAME)
	go build -ldflags="-s -w" -o $(BINDIR)/$(APP_NAME) ./cmd/beacon
	@echo "✓ $(APP_NAME) installed to $(BINDIR)/$(APP_NAME)"

test:
	go test -race -count=1 ./...

clean:
	rm -rf bin/
