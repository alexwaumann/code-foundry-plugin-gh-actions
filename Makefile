# Build, test and package the plugin. NAME and VERSION are read from plugin.json's
# top-level "name" and "version" lines (keep them on lines of their own, two-space
# indented).
NAME    := $(shell sed -n 's/^  "name": "\(.*\)",$$/\1/p' plugin.json)
VERSION := $(shell sed -n 's/^  "version": "\(.*\)",$$/\1/p' plugin.json)

.PHONY: build test dist clean

## build: the executable plugin.json's exec names, for `code-foundry plugin dev`.
build:
	go build -trimpath -o bin/$(NAME) .

## test: the unit tests (plugintest).
test:
	go test ./...

## dist: one release tarball per macOS architecture with plugin.json and bin/<name> at
## its root, and their SHA-256 sums: dist/<name>-<version>-darwin-<arch>.tar.gz,
## dist/SHA256SUMS.
dist:
	rm -rf dist && mkdir -p dist
	for arch in arm64 amd64; do \
		pkg=dist/pkg-$$arch && mkdir -p $$pkg/bin && cp plugin.json $$pkg/ && chmod 0644 $$pkg/plugin.json && \
		GOOS=darwin GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags=-s -o $$pkg/bin/$(NAME) . && \
		COPYFILE_DISABLE=1 tar -C $$pkg -czf dist/$(NAME)-$(VERSION)-darwin-$$arch.tar.gz plugin.json bin/$(NAME) && \
		rm -rf $$pkg || exit 1; \
	done
	cd dist && shasum -a 256 *.tar.gz > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf bin dist
