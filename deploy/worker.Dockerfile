# The workflow worker. Built from the repo root rather than from deploy/ -- see
# the worker service's build.context in docker-compose.yml -- because the binary
# needs cmd/ and internal/, which live above this file.
#
# What lands in the context is an allowlist in the root .dockerignore: go.mod,
# go.sum, cmd/ and internal/. Nothing else, so web/node_modules and any .env
# never reach the daemon.
ARG GO_VERSION
ARG ALPINE_VERSION

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Module download is its own layer so editing workflow code doesn't re-fetch the
# SDK, and the cache mounts persist between builds. Together they keep a rebuild
# after a code change to a few seconds, which is what makes `make worker` usable
# as the edit-restart loop.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/worker ./cmd/worker

FROM alpine:${ALPINE_VERSION}
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["worker"]
