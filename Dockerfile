# syntax=docker/dockerfile:1
# Go version: keep in step with .mise.toml.
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/quyendv/opsward/internal/version.Version=${VERSION} -X github.com/quyendv/opsward/internal/version.Commit=${COMMIT}" \
      -o /out/opsward ./cmd/opsward

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/opsward /opsward
USER nonroot:nonroot
ENTRYPOINT ["/opsward"]
