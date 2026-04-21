# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS builder
WORKDIR /src

# Pre-populate the module cache so source edits do not invalidate it.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static build: distroless/static has no libc, so CGO must be off.
# -trimpath keeps the image reproducible; -s -w strips the DWARF and
# symbol tables to shrink the binary.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
        -o /out/vsockd ./cmd/vsockd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/vsockd /vsockd
EXPOSE 9090
ENTRYPOINT ["/vsockd", "-config", "/etc/vsockd/vsockd.yaml"]
