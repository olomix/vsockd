BINARY := vsockd
PKG    := github.com/olomix/vsockd
DOCKER_IMAGE := vsockd:dev

GOFLAGS  ?=
LDFLAGS  ?= -s -w
BUILDFLAGS := -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test vet lint docker clean

build:
	CGO_ENABLED=0 go build $(BUILDFLAGS) -o $(BINARY) ./cmd/vsockd

test:
	go test ./...

vet:
	go vet ./...

lint:
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "staticcheck not installed; run: go install honnef.co/go/tools/cmd/staticcheck@latest"; \
		exit 1; \
	fi

docker:
	docker build -t $(DOCKER_IMAGE) .

clean:
	rm -f $(BINARY)
	rm -rf dist/
