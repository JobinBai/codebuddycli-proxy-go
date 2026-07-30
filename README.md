# codebuddycli-proxy-go

纯 Go、零第三方运行时依赖的 CodeBuddy OpenAI Chat Completions 兼容代理。它直连 CodeBuddy 上游，支持 SSE、非流式聚合、模型列表、代理访问令牌，以及与 CodeBuddy CLI 对齐的鉴权和会话请求头。

## 快速开始

```bash
export CODEBUDDY_API_KEY='你的 CodeBuddy API Key'
export CODEBUDDY_INTERNET_ENVIRONMENT=internal
go run ./cmd/codebuddycli-proxy
```

NAS（x86）使用 Docker：

```bash
docker run -d --name codebuddycli-proxy --restart unless-stopped -p 8787:8787 \
  -e CODEBUDDY_API_KEY -e CODEBUDDY_INTERNET_ENVIRONMENT=internal \
  ghcr.io/<owner>/codebuddycli-proxy-go:latest
```

`POST /v1/chat/completions` 和 `GET /v1/models` 均兼容 OpenAI 客户端；`GET /health` 用于健康检查。`PROXY_API_KEY` 设置后，业务接口必须携带 `Authorization: Bearer <key>`。

## 构建与发布

```bash
make test
make build VERSION=v2.0.0
make cross VERSION=v2.0.0
make docker VERSION=v2.0.0
```

`make cross` 会生成 Linux、macOS、Windows 的 amd64/arm64 发布包，包内可执行文件均为 `codebuddycli-proxy`（Windows 为 `.exe`）。推送 `v*` 标签会自动创建 GitHub Release、上传发布包，并发布 `linux/amd64` 与 `linux/arm64` 的 GHCR Docker 镜像。
