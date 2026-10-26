# syntax=docker/dockerfile:1.7

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/simulator .

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/simulator /app/simulator
USER nonroot:nonroot
ENTRYPOINT ["/app/simulator"]
