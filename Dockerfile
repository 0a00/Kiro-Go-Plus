# builder 阶段始终运行在构建机原生平台（amd64），用 Go 交叉编译目标平台二进制
FROM --platform=$BUILDPLATFORM golang:1.23-alpine3.21 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o kiro-go .

FROM alpine:3.21
RUN apk --no-cache add ca-certificates \
    && addgroup -S -g 1000 kiro \
    && adduser -S -D -H -u 1000 -G kiro kiro

WORKDIR /app
COPY --from=builder --chown=kiro:kiro /app/kiro-go .
COPY --from=builder --chown=kiro:kiro /app/web ./web
RUN mkdir -p /app/data && chown kiro:kiro /app/data

EXPOSE 8080 3128
VOLUME /app/data

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:${HEALTHCHECK_PORT:-8080}/health || exit 1

USER kiro
CMD ["./kiro-go"]
