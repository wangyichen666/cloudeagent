# Agent 实例运行时镜像：多阶段构建。
# 最终镜像 = agent-runtime（Go，ACP 客户端网关）+ QwenPaw ACP 内核（Python）。
# 对应文档「数据面每用户 Pod」的镜像；模型凭证由控制面运行时注入（env），
# 绝不打入镜像，也不写入任何持久卷。
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/agent-runtime ./cmd/agent-runtime

FROM python:3.12-slim
# QwenPaw ACP 内核（pip 需要网络；构建时间会较长，镜像体积较大）。
# 构建产物体积与依赖说明见 README「QwenPaw 内核接入」。
ARG QPW_VERSION=latest
RUN if [ "$QPW_VERSION" = "latest" ]; then \
        pip install --no-cache-dir qwenpaw; \
    else \
        pip install --no-cache-dir "qwenpaw==${QPW_VERSION}"; \
    fi \
    && adduser --disabled-password --gecos "" --uid 1000 --home /workspace agent

COPY --from=builder /out/agent-runtime /usr/local/bin/agent-runtime
WORKDIR /workspace
USER agent
EXPOSE 18585
ENTRYPOINT ["/usr/local/bin/agent-runtime"]
CMD ["--listen", "0.0.0.0:18585", "--workspace", "/workspace", "--qwenpaw-bin", "/usr/local/bin/qwenpaw"]
