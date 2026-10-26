# syntax=docker/dockerfile:1.7

# --- build: compile a static binary ------------------------------------------
FROM golang:1.22-alpine AS build
WORKDIR /src

# Dependencies first so they are cached independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# timetzdata embeds the zone database so QUEUE_TIMEZONE works without OS tzdata.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/api ./cmd/api

# --- runtime: no shell, no package manager, non-root -------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/api /app/api
USER nonroot:nonroot
EXPOSE 8080
# The image has no curl; the binary probes its own /health.
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=12 CMD ["/app/api", "healthcheck"]
ENTRYPOINT ["/app/api"]
