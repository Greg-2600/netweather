# syntax=docker/dockerfile:1

# Build stage: Chainguard's Go image, static binary, version stamped at build.
# Pinned by digest (THREAT_MODEL.md T61/T63); Dependabot opens a weekly PR with
# the new digest, and the comment keeps the upstream tag readable.
FROM cgr.dev/chainguard/go:latest@sha256:97b081e091e2c7df54b30f468969ea2e62d7d1a64fff89346facb564da979300 AS build

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
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:6ab86c5e059da41ff1454b6f69ecd7422b94af41969f4e231aa23244e47d4a81 AS debug

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
FROM cgr.dev/chainguard/static:latest@sha256:54b5f14edad6f2554774d499539e837588a501bd5bf22ac6cd97b1068c35d44d

COPY --from=build /out/netweather /netweather
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
EXPOSE 2600

ENV NETWEATHER_UI_LISTEN=0.0.0.0:2600 \
    NETWEATHER_DATA=/data

VOLUME ["/data"]

ENTRYPOINT ["/netweather"]
CMD ["serve"]
