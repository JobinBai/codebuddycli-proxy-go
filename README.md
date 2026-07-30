# codebuddycli-proxy-go

纯 Go、零第三方运行时依赖的 CodeBuddy OpenAI Chat Completions 兼容代理。它直连 CodeBuddy 上游，支持 SSE、非流式聚合、模型列表，以及与 CodeBuddy CLI 对齐的鉴权和会话请求头。

## 快速开始

```bash
export CODEBUDDY_API_KEY='你的 CodeBuddy API Key'
export CODEBUDDY_INTERNET_ENVIRONMENT=internal
go run ./cmd/codebuddycli-proxy
```

使用 Docker：

```bash
docker run -d --name codebuddycli-proxy --restart unless-stopped -p 8787:8787 \
  -e CODEBUDDY_API_KEY -e CODEBUDDY_INTERNET_ENVIRONMENT=internal \
  jobinbai/codebuddycli-proxy-go:latest
```

`POST /v1/chat/completions` 和 `GET /v1/models` 均兼容 OpenAI 客户端；`GET /health` 用于健康检查。
