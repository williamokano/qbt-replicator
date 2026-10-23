# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=none DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w \
        -X github.com/williamokano/qbt-replicator/internal/version.Version=${VERSION} \
        -X github.com/williamokano/qbt-replicator/internal/version.Commit=${COMMIT} \
        -X github.com/williamokano/qbt-replicator/internal/version.Date=${DATE}" \
      -o /out/qbt-replicator ./cmd/qbt-replicator
# Pre-create /config owned by nonroot so fresh named volumes are writable (state.json).
RUN mkdir -p /out/config

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="qbt-replicator" \
      org.opencontainers.image.source="https://github.com/williamokano/qbt-replicator" \
      org.opencontainers.image.description="Replicate completed torrents across qBittorrent instances sharing storage"
COPY --from=build /out/qbt-replicator /qbt-replicator
COPY --from=build --chown=65532:65532 /out/config /config
ENV QBTR_CONFIG=/config/config.yaml
VOLUME ["/config"]
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/qbt-replicator", "healthcheck"]
ENTRYPOINT ["/qbt-replicator"]
CMD ["serve"]
