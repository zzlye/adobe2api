# 构建后仅保留运行文件，减少部署镜像中的无用内容。
# 编译阶段固定使用构建机架构，避免 ARM64 镜像构建时通过 QEMU 编译 Go。
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/adobe2api .

FROM debian:bookworm-slim
# 浏览器仅在账号刷新与上游请求期间使用，其依赖由安装程序统一处理。
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates chromium \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 adobe2api \
    && useradd --uid 10001 --gid 10001 --create-home adobe2api \
    && mkdir -p /app/data \
    && chown -R adobe2api:adobe2api /app
WORKDIR /app
COPY --from=build /out/adobe2api /app/adobe2api
ENV ADOBE2API_ADDR=:8080 \
    ADOBE2API_DATA_DIR=/app/data \
    ADOBE2API_CHROME=/usr/bin/chromium \
    ADOBE2API_CHROME_NO_SANDBOX=1 \
    GOMEMLIMIT=256MiB
USER adobe2api
EXPOSE 8080
VOLUME ["/app/data"]
ENTRYPOINT ["/app/adobe2api"]
