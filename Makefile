VERSION ?= 0.1.0-dev
# SIGN_KEY=<private key from towamori-sign keygen> also writes a .sig next to every archive,
# which is what lets running agents accept the build when the console offers it.
SIGN_KEY ?=
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
	if [ -n "$(SIGN_KEY)" ]; then set -e; for arch in $(ARCHES); do \
		go run ./cmd/towamori-sign sign -key "$(SIGN_KEY)" -version "$(VERSION)" -os linux -arch $$arch dist/towamori-agent_linux_$$arch.tar.gz; \
	done; fi

clean:
	rm -rf bin dist
