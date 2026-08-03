# The worker binary. Built from the repo root -- see build.context in
# docker-compose.yml -- because it needs cmd/ and internal/, which live above
# this file. What lands in the context is an allowlist in the root
# .dockerignore.
#
# Keep golang in step with go.mod's toolchain directive -- on a lower patch
# the build still works, but GOTOOLCHAIN=auto fetches the right one every time.
FROM golang:1.25.12-alpine AS build
WORKDIR /src

# Module download is its own layer, and the cache mounts persist between
# builds, so a rebuild after a code change takes seconds -- which is what
# makes `make worker` usable as the edit-restart loop.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/worker ./cmd/worker

FROM alpine:3.22
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["worker"]
