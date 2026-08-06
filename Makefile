GO ?= go
GOFLAGS ?=
BIN_DIR := bin

.PHONY: all build control-plane agent-runtime test vet tidy demo k8s

all: build

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/control-plane ./cmd/control-plane
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/agent-runtime ./cmd/agent-runtime

control-plane:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/control-plane ./cmd/control-plane

agent-runtime:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/agent-runtime ./cmd/agent-runtime

test:
	$(GO) test $(GOFLAGS) ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

# 本地一键演示：进程后端 + mock LLM + 内存存储，零外部依赖
demo:
	./scripts/demo.sh

# 编译 K8s 演进路径的后端模块（单独 module，生产环境接入）
k8s:
	cd k8sbackend && $(GO) build ./...
