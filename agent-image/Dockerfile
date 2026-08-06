# Agent 实例运行时镜像：多阶段构建，最终镜像仅包含二进制与 CA 证书。
# 对应文档「数据面每用户 Pod」的镜像；模型凭证由控制面运行时注入，绝不打入镜像。
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/agent-runtime ./cmd/agent-runtime

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 -h /workspace agent
WORKDIR /workspace
COPY --from=builder /out/agent-runtime /usr/local/bin/agent-runtime
USER agent
EXPOSE 18585
ENTRYPOINT ["/usr/local/bin/agent-runtime"]
CMD ["--listen", "0.0.0.0:18585", "--workspace", "/workspace"]
