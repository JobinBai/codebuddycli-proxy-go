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

默认显示模型思考过程（`HIDE_REASONING=0`）。代理会在客户端未显式传入时自动注入推理参数（`reasoning_effort` 与 `reasoning`），因此普通对话默认即可见思考。图片请求将上游分片思考合并为单个 `reasoning_content` 事件；纯文本请求采用**有界窗口合并（coalesce）**：第一个思考分片立即发出（零感知延迟），随后在时间窗口或字节阈值内把上游多个分片合并成更少事件，既不退化为整段等待（aggregate 的痛点），也不逐片透传（客户端会拆成上百张卡）。设置 `HIDE_REASONING=1` 可彻底关闭推理（不注入、不显示）。

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `HIDE_REASONING` | `0` | 为 `1` 时隐藏并关闭思考过程 |
| `REASONING_COALESCE` | `1` | 纯文本请求的窗口合并开关；为 `0` 时退化为旧版逐片透传 |
| `REASONING_COALESCE_MS` | `250` | 合并窗口时长（毫秒）；窗口到期或上游停顿时主动刷新已缓冲的思考 |
| `REASONING_COALESCE_CHARS` | `1024` | 缓冲字节阈值；达到即刷新，同时限制事件数与内存 |

> 注意：窗口合并只能**减少**客户端思考卡数量，无法把一次长思考压成单卡。若客户端仍按每事件一张卡渲染，需在客户端按 `id + choice.index` 累积 `reasoning_content` 才是根本解法。调大 `REASONING_COALESCE_MS`（如 500–1000）可进一步减少事件数。
