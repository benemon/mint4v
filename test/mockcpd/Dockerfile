# Mock CP4D API server for the e2e suite. Built from the repo root:
#   docker build -f test/mockcpd/Dockerfile -t mockcpd:e2e .
FROM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY test/mockcpd/ test/mockcpd/
RUN CGO_ENABLED=0 go build -trimpath -o mockcpd ./test/mockcpd

FROM registry.access.redhat.com/ubi9/ubi-micro:9.9@sha256:dbd325b96bcd432dd42816d31412bdf1dc0fb973b79ed310344bc23c76c068db
COPY --from=builder /workspace/mockcpd /usr/local/bin/mockcpd
USER 1001
ENTRYPOINT ["/usr/local/bin/mockcpd"]
