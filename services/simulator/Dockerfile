# syntax=docker/dockerfile:1

# ---- build: static binary, cross-compiled on the build machine for each target platform ----
# --platform=$BUILDPLATFORM runs the Go toolchain natively (no QEMU emulation); GOOS/GOARCH
# below pick the target, so `docker buildx build --platform linux/amd64,linux/arm64` is fast.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY scenarios ./scenarios
ARG APP_VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/simulator ./cmd/simulator

# ---- runtime: distroless, no shell, non-root ----
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/simulator /simulator
USER nonroot:nonroot
# The state file holds seeded API keys and courier tokens. /tmp is the one writable place
# under a read-only root filesystem (a tmpfs in Compose, an emptyDir in Kubernetes).
ENV STATE_FILE=/tmp/simulator/state.json
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD ["/simulator", "healthcheck"]
ENTRYPOINT ["/simulator"]
# Scenarios are compiled in: override with e.g. ["run", "-scenario", "rainy-friday"].
CMD ["run", "-scenario", "small"]
