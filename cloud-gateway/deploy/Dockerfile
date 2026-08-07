# cloud-gateway 数据面网关镜像。
# 构建前先编译二进制：
#   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/gateway ./cmd/gateway
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 -h /gateway gateway
COPY bin/gateway /usr/local/bin/gateway
USER gateway
EXPOSE 18500
ENTRYPOINT ["/usr/local/bin/gateway"]
