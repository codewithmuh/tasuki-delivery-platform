# syntax=docker/dockerfile:1

# One Dockerfile for every binary in this module:
#   docker build --build-arg SERVICE=fleet-ingest -t fleet-ingest .
# SERVICE is one of the directories under cmd/ (fleet-ingest, fleet-state, tracking).

# ---- build: static binary, no cgo, cross-compiled on the build machine's own architecture ----
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG SERVICE
RUN test -n "$SERVICE" && test -d "cmd/$SERVICE" \
    || { echo "build with --build-arg SERVICE=<one of: $(ls cmd | tr '\n' ' ')>" >&2; exit 1; }
ARG TARGETOS TARGETARCH
ARG APP_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/app "./cmd/${SERVICE}"

# ---- runtime: distroless, no shell, non-root (UID 65532), works with a read-only root filesystem ----
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/app", "healthcheck"]
ENTRYPOINT ["/app"]
