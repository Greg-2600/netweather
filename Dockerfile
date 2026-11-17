# syntax=docker/dockerfile:1

# Build stage: Chainguard's Go image, static binary, version stamped at build.
# Pinned by digest (THREAT_MODEL.md T61/T63); Dependabot opens a weekly PR with
# the new digest, and the comment keeps the upstream tag readable.
FROM cgr.dev/chainguard/go:latest@sha256:78352255a8760797dac56865a5b9af2881f245ef74c90199e4e15f52251c5357 AS build

USER 0
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
      -o /out/netweather ./cmd/netweather

# A directory that exists in the image with the runtime uid is what makes a
# Docker named volume usable: Docker copies the path's ownership on first use.
# Without this, the non-root process cannot create /data/netweather.db.
RUN mkdir -p /out/data

# Runtime: static, no shell, no package manager, no libc beyond what Go needs.
# Pinned by digest for the same reason as the builder.
FROM cgr.dev/chainguard/static:latest@sha256:fc42af33e643f20d08baf03cddcc1c02ba57052dfb02edb0d79f3fc47bcea8d9

COPY --from=build /out/netweather /netweather
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
EXPOSE 2600

ENV NETWEATHER_UI_LISTEN=0.0.0.0:2600 \
    NETWEATHER_DATA=/data

VOLUME ["/data"]

ENTRYPOINT ["/netweather"]
CMD ["serve"]
