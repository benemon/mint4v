# Build the mint4v binary
FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o mint4v ./cmd

# Runtime: Red Hat UBI micro, pinned to the current minor release.
# Bump deliberately; consider pinning by digest for release builds.
FROM registry.access.redhat.com/ubi9/ubi-micro:9.8@sha256:d3499a0896e2d6a1642f922615831bc5c2fd513bbb34f3446cabba530d9d54a2

COPY --from=builder /workspace/mint4v /usr/local/bin/mint4v

# Any non-root UID works; OpenShift's restricted-v2 SCC will assign its own.
USER 1001

ENTRYPOINT ["/usr/local/bin/mint4v"]
