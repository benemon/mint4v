# Mock CP4D API server for the e2e suite. Built from the repo root:
#   docker build -f test/mockcpd/Dockerfile -t mockcpd:e2e .
FROM golang:1.27@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY test/mockcpd/ test/mockcpd/
RUN CGO_ENABLED=0 go build -trimpath -o mockcpd ./test/mockcpd

FROM registry.access.redhat.com/ubi9/ubi-micro:9.6@sha256:990002083442f6a93cd3249da32ecb7c3f6be778a1bec3a73a9c17fbc40edc15
COPY --from=builder /workspace/mockcpd /usr/local/bin/mockcpd
USER 1001
ENTRYPOINT ["/usr/local/bin/mockcpd"]
