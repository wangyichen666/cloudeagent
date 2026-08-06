# 集群内控制面镜像：control-plane-k8s（K8s StatefulSet 后端）。
# 构建前先编译二进制：
#   cd k8sbackend && CGO_ENABLED=0 go build -o ../bin/control-plane-k8s ./cmd/control-plane
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 -h /control-plane control
COPY bin/control-plane-k8s /usr/local/bin/control-plane-k8s
USER control
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/control-plane-k8s"]
