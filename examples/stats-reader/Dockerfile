# syntax=docker/dockerfile:1.4

# Build stage
FROM registry.access.redhat.com/ubi9/go-toolset:1.25 AS builder

WORKDIR /build
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY examples/stats-reader/main.go ./

RUN --mount=type=cache,target=/go-cache,uid=1001,gid=0 \
    --mount=type=cache,target=/go-mod-cache,uid=1001,gid=0 \
    CGO_ENABLED=0 GOCACHE=/go-cache GOMODCACHE=/go-mod-cache go build -o stats-reader .

# Runtime image. This is example code; the tag is deliberately
# floating so the sample keeps working against whatever ubi-minimal
# ships at the time.
# hadolint ignore=DL3007
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

COPY --from=builder /build/stats-reader /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/stats-reader"]
