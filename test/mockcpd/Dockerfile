# Mock CP4D API server for the e2e suite. Built from the repo root:
#   docker build -f test/mockcpd/Dockerfile -t mockcpd:e2e .
FROM golang:1.26@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY test/mockcpd/ test/mockcpd/
RUN CGO_ENABLED=0 go build -trimpath -o mockcpd ./test/mockcpd

FROM registry.access.redhat.com/ubi9/ubi-micro:9.6@sha256:990002083442f6a93cd3249da32ecb7c3f6be778a1bec3a73a9c17fbc40edc15
COPY --from=builder /workspace/mockcpd /usr/local/bin/mockcpd
USER 1001
ENTRYPOINT ["/usr/local/bin/mockcpd"]
