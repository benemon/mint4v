# Mock CP4D API server for the e2e suite. Built from the repo root:
#   docker build -f test/mockcpd/Dockerfile -t mockcpd:e2e .
FROM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY test/mockcpd/ test/mockcpd/
RUN CGO_ENABLED=0 go build -trimpath -o mockcpd ./test/mockcpd

FROM registry.access.redhat.com/ubi9/ubi-micro:9.8@sha256:5be877a2a280ae36eb9ab80ca4efb28bbf77c57abbba38137cdc37a03b8465e6
COPY --from=builder /workspace/mockcpd /usr/local/bin/mockcpd
USER 1001
ENTRYPOINT ["/usr/local/bin/mockcpd"]
