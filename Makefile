VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e install release clean

build:
	go build -ldflags "$(LDFLAGS)" -o hull ./cmd/hull

test:
	go vet ./...
	go test ./...

e2e: build
	sudo scripts/e2e.sh ./hull

install: build
	install -m 0755 hull /usr/local/bin/hull

release:
	mkdir -p dist
	for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		GOOS=$${t%/*} GOARCH=$${t#*/} CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/hull-$${t%/*}-$${t#*/} ./cmd/hull; \
	done

clean:
	rm -rf hull dist
