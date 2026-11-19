# syntax=docker/dockerfile:1.7

# ---- build ---------------------------------------------------------------
# Runs on the build machine's architecture and cross-compiles (pure Go, no cgo),
# so multi-arch images build fast without emulation.
ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go tool templ generate \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/yt-subscriptions . \
 && mkdir -p /out/data

# ---- runtime -------------------------------------------------------------
# Distroless static: CA certificates and a nonroot user, no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="yt-subscriptions" \
      org.opencontainers.image.description="Review your YouTube subscriptions with AI suggestions" \
      org.opencontainers.image.source="https://github.com/bograh/yt-subscriptions" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/yt-subscriptions /app/yt-subscriptions
COPY --from=build --chown=nonroot:nonroot /out/data /data

ENV HOST=0.0.0.0 \
    PORT=8080 \
    DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/yt-subscriptions", "-healthcheck"]
ENTRYPOINT ["/app/yt-subscriptions"]
