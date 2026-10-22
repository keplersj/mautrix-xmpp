# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# mautrix-xmpp container image.
#
#   docker build -t mautrix-xmpp .
#   docker run --rm --user 10001 --read-only \
#     -v /path/to/config.yaml:/data/config.yaml:ro -v /path/to/data:/data \
#     mautrix-xmpp
#
# Image contract (relied on by Helm charts):
#   - static binary at /usr/local/bin/mautrix-xmpp, no shell, no package manager
#   - runs as any non-root uid (default 10001), needs no home directory
#   - config is read from /data/config.yaml and may be read-only
#   - /data is the only path that has to be writable (SQLite DB, logs)
#   - no inbound ports when homeserver.websocket is true (bbctl's default)
#
# The bridge needs cgo: mautrix-go's bridge runner links mattn/go-sqlite3.
# It is built against musl and linked statically, cross-compiled with xx so
# multi-arch builds don't need emulation.

FROM --platform=$BUILDPLATFORM tonistiigi/xx:1.6.1@sha256:923441d7c25f1e2eb5789f82d987693c47b8ed987c4ab3b075d6ed2b5d6779a3 AS xx

FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder
COPY --from=xx / /
RUN apk add --no-cache clang lld
ARG TARGETPLATFORM
RUN xx-apk add --no-cache musl-dev gcc

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download

COPY . .
ARG VERSION=unknown
ARG COMMIT=unknown
ARG BUILDTIME=unknown
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    MAUTRIX_VERSION="$(awk '$1 == "maunium.net/go/mautrix" { print $2 }' go.mod)" && \
    xx-go build -trimpath \
        -tags goolm,netgo,osusergo,sqlite_omit_load_extension \
        -ldflags "-s -w -linkmode external -extldflags '-static' \
            -X main.Tag=${VERSION} -X main.Commit=${COMMIT} -X 'main.BuildTime=${BUILDTIME}' \
            -X maunium.net/go/mautrix.GoModVersion=${MAUTRIX_VERSION}" \
        -o /out/mautrix-xmpp ./cmd/mautrix-xmpp && \
    xx-verify --static /out/mautrix-xmpp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/mautrix-xmpp /usr/local/bin/mautrix-xmpp
USER 10001:10001
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/mautrix-xmpp"]
CMD ["-c", "/data/config.yaml"]
