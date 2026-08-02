# The Go binaries -- worker, api and the seed one-shot. One file for all three,
# selected by CMD, because the only difference is which package under cmd/ gets
# built.
#
# Built from the repo root rather than from deploy/ -- see build.context in
# docker-compose.yml -- because the binaries need cmd/ and internal/, which live
# above this file. What lands in the context is an allowlist in the root
# .dockerignore: go.mod, go.sum, cmd/ and internal/. Nothing else, so
# web/node_modules and .git never reach the daemon.
#
# The two base images are pinned here rather than in docker-compose.yml, unlike
# the server images: they are a property of how this repo builds, not of a stack.
# Keep golang in step with go.mod's toolchain directive -- on a lower patch the
# build still works, but GOTOOLCHAIN=auto fetches the right one every time.
FROM golang:1.25.12-alpine AS build
ARG CMD
WORKDIR /src

# Module download is its own layer so editing workflow code doesn't re-fetch the
# SDK, and the cache mounts persist between builds. Together they keep a rebuild
# after a code change to a few seconds, which is what makes `make worker` and
# `make api` usable as the edit-restart loop. All three share both the layer and
# the caches, so each image after the first is nearly free.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/service ./cmd/${CMD}

FROM alpine:3.22
COPY --from=build /out/service /usr/local/bin/service
ENTRYPOINT ["service"]
