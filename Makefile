VERSION ?= 0.6.17
LDFLAGS = -ldflags "-X main.version=$(VERSION)"

.PHONY: run build clean package deb rpm

run:
	CGO_ENABLED=1 go run $(LDFLAGS) ./cmd/server/

build:
	CGO_ENABLED=1 go build $(LDFLAGS) -o vocala ./cmd/server/

clean:
	rm -f vocala vocala.db vocala.db-wal vocala.db-shm
	rm -rf dist/

deb: build
	mkdir -p dist
	nfpm package --packager deb --target dist/

rpm: build
	mkdir -p dist
	nfpm package --packager rpm --target dist/

package: deb rpm
