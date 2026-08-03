# The Go binaries -- worker, api and the seed one-shot -- one file for all
# three, selected by CMD. Built from the repo root (see build.context in
# docker-compose.yml) because the binaries need cmd/ and internal/; the root
# .dockerignore allowlists what reaches the daemon.
#
# Keep golang in step with go.mod's toolchain directive.
FROM golang:1.25.12-alpine AS build
ARG CMD
WORKDIR /src

# Module download is its own layer, and the cache mounts persist between
# builds, so a rebuild after a code change takes a few seconds -- which is
# what makes `make worker` / `make api` usable as the edit-restart loop.
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
