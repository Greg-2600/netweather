# syntax=docker/dockerfile:1

# Build stage: Chainguard's Go image, static binary, version stamped at build.
# Pinned by digest (THREAT_MODEL.md T61/T63); Dependabot opens a weekly PR with
# the new digest, and the comment keeps the upstream tag readable.
FROM cgr.dev/chainguard/go:latest@sha256:aef3819582d3638863f214540c076b312542c48588b59841290ea74b1743d5ac AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/Greg-2600/netweather/internal/version.Version=${VERSION} \
        -X github.com/Greg-2600/netweather/internal/version.Commit=${COMMIT} \
        -X github.com/Greg-2600/netweather/internal/version.Date=${DATE}" \
      -o /out/netweather ./cmd/netweather \
 && mkdir -p /out/data

# Debug variant (docs/IMAGE.md §1): the same binary plus a shell and the network
# tools an operator needs on a remote instance. It is deliberately a second tag,
# never the default: the release image has no shell. Pinned by digest like the
# other stages.
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:28c39be5a5e1e9b85081c10740e8495f6518a865b23bc6a486637c4b21e74b43 AS debug

# Wolfi's own repository carries the debugging tools; the base image points at
# Chainguard's package repo, which does not carry them. `traceroute` is not a
# separate Wolfi package, but the installed set provides the binary.
# hadolint ignore=DL3018
RUN echo 'https://packages.wolfi.dev/os' >> /etc/apk/repositories \
 && apk add --no-cache ca-certificates-bundle tzdata bind-tools iputils tcpdump curl netcat-openbsd \
 && mkdir -p /data \
 && chown 65532:65532 /data

COPY --from=build /out/netweather /netweather

USER 65532:65532
EXPOSE 2600

ENV NETWEATHER_UI_LISTEN=0.0.0.0:2600 \
    NETWEATHER_DATA=/data

VOLUME ["/data"]

ENTRYPOINT ["/netweather"]
CMD ["serve"]

# Runtime: static, no shell, no package manager, no libc beyond what Go needs.
# Pinned by digest for the same reason as the builder. This stays the last stage
# so a plain `docker build` produces the release image, not the debug one.
FROM cgr.dev/chainguard/static:latest@sha256:599c3cd7597b84aa149c4282beecc8c6d4e1cb5c95dc8d982e70cd0a2a9147a2

COPY --from=build /out/netweather /netweather
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
EXPOSE 2600

ENV NETWEATHER_UI_LISTEN=0.0.0.0:2600 \
    NETWEATHER_DATA=/data

VOLUME ["/data"]

ENTRYPOINT ["/netweather"]
CMD ["serve"]
