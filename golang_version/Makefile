# 赛尔号 Golang 版本构建脚本

# 项目名称
PROJECT_NAME := seer-golang

# 构建输出目录
OUTPUT_DIR := output

# Go 编译参数
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
GOFLAGS := -ldflags "-s -w"

# 服务器列表
SERVERS := gameserver loginserver ressrv loginip

# 默认目标
all: build

# 构建所有服务器
build: $(SERVERS)
	@echo "构建完成！"

# 构建单个服务器
$(SERVERS):
	@echo "构建 $@ 服务器..."
	@mkdir -p $(OUTPUT_DIR)
	@go build $(GOFLAGS) -o $(OUTPUT_DIR)/$@ ./cmd/$@
	@echo "$@ 服务器构建完成"

# 运行游戏服务器
run-gameserver:
	@echo "启动游戏服务器..."
	@go run ./cmd/gameserver

# 运行登录服务器
run-loginserver:
	@echo "启动登录服务器..."
	@go run ./cmd/loginserver

# 运行资源服务器
run-ressrv:
	@echo "启动资源服务器..."
	@go run ./cmd/ressrv

# 运行登录IP服务器
run-loginip:
	@echo "启动登录IP服务器..."
	@go run ./cmd/loginip

# 清理构建产物
clean:
	@echo "清理构建产物..."
	@rm -rf $(OUTPUT_DIR)
	@echo "清理完成"

# 安装依赖
install:
	@echo "安装依赖..."
	@go mod tidy
	@echo "依赖安装完成"

# 格式化代码
fmt:
	@echo "格式化代码..."
	@go fmt ./...
	@echo "代码格式化完成"

# 运行测试
test:
	@echo "运行测试..."
	@go test ./...
	@echo "测试完成"

# 检查代码质量
lint:
	@echo "检查代码质量..."
	@go vet ./...
	@echo "代码质量检查完成"

.PHONY: all build $(SERVERS) run-gameserver run-loginserver run-ressrv run-loginip clean install fmt test lint
