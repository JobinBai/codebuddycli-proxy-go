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

模型列表优先从 CodeBuddy 远端配置发现，并按 `MODEL_REFRESH_INTERVAL_MS`（默认 10 分钟）缓存；远端未提供账号模型列表时自动使用内置兼容列表。访问 `GET /v1/models?refresh=1` 可手动刷新，响应头 `X-CodeBuddy-Models-Source` 会标明 `remote` 或 `fallback`。

默认显示模型思考过程（`HIDE_REASONING=0`）。图片请求会将上游分片思考合并为一个 `reasoning_content` 事件，避免 Cherry Studio 拆成多张思考卡；纯文本请求保留实时逐片输出。设置 `HIDE_REASONING=1` 可隐藏思考过程。
