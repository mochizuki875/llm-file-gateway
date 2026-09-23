# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -mod=readonly -trimpath -ldflags="-s -w" \
        -o /out/llm-file-gateway ./cmd/llm-file-gateway

FROM debian:bookworm-slim

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        fontconfig \
        fonts-crosextra-carlito \
        fonts-liberation \
        fonts-noto-cjk \
        libreoffice-calc \
        libreoffice-impress \
        libreoffice-writer \
    && rm -rf /var/lib/apt/lists/*

COPY .devcontainer/fonts.conf /etc/fonts/local.conf

RUN fc-cache -f \
    && useradd --create-home --home-dir /home/gateway --no-log-init \
        --shell /usr/sbin/nologin --uid 10001 gateway \
    && install -d -o gateway -g gateway /var/lib/file-gateway

COPY --from=build /out/llm-file-gateway /usr/local/bin/llm-file-gateway

USER gateway
ENV HOME=/home/gateway \
    GATEWAY_HOST=0.0.0.0 \
    GATEWAY_PORT=8080 \
    GATEWAY_DATA_DIR=/var/lib/file-gateway
WORKDIR /home/gateway
EXPOSE 8080

ENTRYPOINT ["llm-file-gateway"]
