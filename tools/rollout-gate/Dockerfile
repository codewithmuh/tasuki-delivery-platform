# syntax=docker/dockerfile:1

# ---- build: cross-compile on the build host's architecture (no QEMU), static binary, no cgo ----
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
ARG APP_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/rollout-gate ./cmd/rollout-gate

# ---- runtime: distroless, no shell, non-root ----
# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64 tools/rollout-gate
# No HEALTHCHECK: the gate runs only in Kubernetes, whose probes call /healthz and /readyz directly.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/rollout-gate /rollout-gate
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/rollout-gate"]
CMD ["serve"]
