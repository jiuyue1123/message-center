GO   ?= go
PKGS ?= ./...

COVERAGE := coverage.out

.DEFAULT_GOAL := help

.PHONY: build
build: ## 编译所有包
	$(GO) build $(PKGS)

.PHONY: test
test: ## 运行测试，开启竞态检测
	$(GO) test -race -covermode=atomic -coverprofile=$(COVERAGE) $(PKGS)

.PHONY: vet
vet: ## 运行 go vet
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt: ## 格式化代码
	$(GO) fmt $(PKGS)

.PHONY: fmt-check
fmt-check: ## 校验代码格式，未通过时列出文件
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "$$unformatted"; exit 1; fi

.PHONY: check
check: fmt-check vet build test ## 依次执行格式校验、vet、编译、测试

.PHONY: cover
cover: ## 在浏览器中打开覆盖率报告
	$(GO) tool cover -html=$(COVERAGE)

.PHONY: deps
deps: ## 校验未引入第三方依赖
	@external=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' $(PKGS) \
	  | grep -v '^$$' | grep -v '^github.com/jiuyue1123/message-center'); \
	if [ -n "$$external" ]; then echo "$$external"; exit 1; fi

.PHONY: tidy
tidy: ## 整理 go.mod
	$(GO) mod tidy

.PHONY: clean
clean: ## 删除构建与测试产物
	$(GO) clean $(PKGS)
	rm -f $(COVERAGE)

.PHONY: help
help: ## 列出所有目标
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
