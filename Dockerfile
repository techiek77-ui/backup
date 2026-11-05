# syntax=docker/dockerfile:1
#
# Build:  docker buildx build --platform linux/amd64 -t <image> .
# OKE:    use linux/amd64 for x86 node pools (VM.Standard.E*, VM.Standard3),
#         linux/arm64 for Ampere A1 node pools, or both for a multi-arch image.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/controller /controller
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/controller"]
