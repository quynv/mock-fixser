VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GEN_STAMP := fix/fix44/.stamp

.PHONY: all gen build test vet clean docker

all: build

# Regenerate fix/ only when the spec or the generator version changes.
$(GEN_STAMP): spec/FIX44.xml go.mod fix/gen.go
	go generate ./fix
	touch $@

## gen: generate the FIX44 Go packages in fix/ from spec/FIX44.xml
gen: $(GEN_STAMP)

## build: build a static binary into bin/mockfix
build: gen
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mockfix ./cmd/mockfix

## test: run all tests with the race detector
test: gen
	go test -race ./...

vet: gen
	go vet ./cmd/... ./internal/...

docker:
	docker build --build-arg VERSION=$(VERSION) -t mockfix:$(VERSION) .

clean:
	rm -rf bin fix/fix44 fix/field fix/tag fix/enum
