VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GEN_STAMP := fix/fix44/.stamp
WEB_DICT := web/src/fix/dict.json
WEB_DEPS := web/node_modules/.package-lock.json

.PHONY: all gen web build build-go dist test test-web e2e vet clean docker

all: build

# Regenerate fix/ only when the spec or the generator version changes.
$(GEN_STAMP): spec/FIX44.xml go.mod fix/gen.go
	go generate ./fix
	touch $@

## gen: generate the FIX44 Go packages in fix/ from spec/FIX44.xml
gen: $(GEN_STAMP)

# The browser's FIX dictionary, also generated from spec/FIX44.xml.
$(WEB_DICT): spec/FIX44.xml cmd/webdict/main.go
	go run ./cmd/webdict -spec spec/FIX44.xml -o $@

$(WEB_DEPS): web/package.json web/package-lock.json
	cd web && npm ci

## web: build the Web UI into web/dist (embedded by the next go build)
web: $(WEB_DICT) $(WEB_DEPS)
	cd web && npm run build

## build: build the UI, then a static binary with the UI embedded
build: web build-go

## build-go: build the binary with whatever is in web/dist (no Node needed)
build-go: gen
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mockfix ./cmd/mockfix

PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

## dist: cross-build release archives in dist/ (run make web first)
dist: gen
	rm -rf dist && mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		name=mockfix_$(VERSION)_$${os}_$${arch}; dir=dist/$$name; \
		echo "build $$name"; mkdir -p $$dir/configs $$dir/data; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$dir/mockfix$$ext ./cmd/mockfix; \
		cp -R spec $$dir/; cp configs/mock.yaml $$dir/configs/; cp data/aapl.csv $$dir/data/; cp README.md LICENSE $$dir/; \
		if [ $$os = windows ]; then (cd dist && zip -qr $$name.zip $$name); \
		else tar -C dist -czf dist/$$name.tar.gz $$name; fi; \
		rm -rf $$dir; \
	done
	cd dist && shasum -a 256 mockfix_* > checksums.txt

## test: run all Go tests with the race detector
test: gen
	go test -race ./...

## test-web: run the UI unit tests (Vitest)
test-web: $(WEB_DICT) $(WEB_DEPS)
	cd web && npm test

## e2e: Playwright test of the Apply flow against a real mockfix
e2e: build
	cd web && npx playwright test

vet: gen
	go vet ./cmd/... ./internal/... ./web

docker:
	docker build --build-arg VERSION=$(VERSION) -t mockfix:$(VERSION) .

clean:
	rm -rf bin fix/fix44 fix/field fix/tag fix/enum $(WEB_DICT)
	find web/dist -mindepth 1 ! -name .gitkeep -delete 2>/dev/null || true
