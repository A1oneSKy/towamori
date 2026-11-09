VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X github.com/towamori/agent/internal/version.Version=$(VERSION)
ARCHES  := amd64 arm64

.PHONY: build test dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/towamori-agent ./cmd/towamori-agent
	go build -o bin/towamori-devserver ./cmd/towamori-devserver

test:
	go vet ./...
	go test -race -count=1 ./...

# Static Linux binaries, one archive per architecture, plus checksums.txt in
# the format install.sh verifies.
dist: clean
	mkdir -p dist
	set -e; for arch in $(ARCHES); do \
		dir=dist/towamori-agent_linux_$$arch; mkdir -p $$dir; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$dir/towamori-agent ./cmd/towamori-agent; \
		cp LICENSE README.md $$dir/; \
		COPYFILE_DISABLE=1 tar --no-xattrs -czf dist/towamori-agent_linux_$$arch.tar.gz -C $$dir towamori-agent LICENSE README.md; \
		rm -r $$dir; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt

clean:
	rm -rf bin dist
