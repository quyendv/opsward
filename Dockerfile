# syntax=docker/dockerfile:1
# Go version: keep in step with .mise.toml.
# The build stage runs on the builder's platform and cross-compiles, so a multi-platform
# build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X github.com/quyendv/opsward/internal/version.Version=${VERSION} -X github.com/quyendv/opsward/internal/version.Commit=${COMMIT}" \
      -o /out/opsward ./cmd/opsward

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/opsward /opsward
USER nonroot:nonroot
ENTRYPOINT ["/opsward"]
