VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PKG_ROOT := github.com/quynv/mock-fixser/fix
GEN_DIRS := fix/fix44 fix/field fix/tag fix/enum

.PHONY: all gen build test vet clean docker

all: build

## gen: regenerate the FIX44 Go packages in fix/ from spec/FIX44.xml
gen:
	rm -rf $(GEN_DIRS)
	mkdir -p fix
	cd fix && go tool generate-fix -pkg-root $(PKG_ROOT) ../spec/FIX44.xml

## build: build a static binary into bin/mockfix
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mockfix ./cmd/mockfix

## test: run all tests with the race detector
test:
	go test -race ./...

vet:
	go vet ./cmd/... ./internal/...

docker:
	docker build --build-arg VERSION=$(VERSION) -t mockfix:$(VERSION) .

clean:
	rm -rf bin
