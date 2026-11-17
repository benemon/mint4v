# Build the mint4v binary
FROM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder
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
FROM registry.access.redhat.com/ubi9/ubi-micro:9.9@sha256:dbd325b96bcd432dd42816d31412bdf1dc0fb973b79ed310344bc23c76c068db

COPY --from=builder /workspace/mint4v /usr/local/bin/mint4v

# Any non-root UID works; OpenShift's restricted-v2 SCC will assign its own.
USER 1001

ENTRYPOINT ["/usr/local/bin/mint4v"]
