# CodeBuddy CLI Proxy 项目交接

更新时间：2026-07-30

## 1. 项目与仓库

### Go 版本（当前主要项目）

- 本地目录：`/Users/jobin/work/demo/codebuddycli-proxy-go`
- GitHub：`git@github.com:JobinBai/codebuddycli-proxy-go.git`
- 当前分支：`main`
- 当前提交：`6be1055 feat: improve image reasoning stream compatibility`
- 当前标签：`v1.0.4`
- 状态：本地与 `origin/main` 一致，交接文档创建前工作树干净
- 可执行文件名：`codebuddycli-proxy`
- Docker 镜像：`jobinbai/codebuddycli-proxy-go:latest`

### Node.js 版本（参考实现）

- 本地目录：`/Users/jobin/work/demo/codebuddycli-proxy`
- 当前 `main`：`2cdc80b`，标签 `v2.0.1`
- `dev`：直连 CodeBuddy 的历史开发分支，远端提交 `f19fb6d`
- `agent-sdk`：保留用于分析 Agent SDK 源码和调用链

## 2. Go 版本当前能力

- 提供 OpenAI 风格的 `POST /v1/chat/completions`
- 提供 `GET /v1/models`
- 提供 `GET /health`
- 支持 SSE 流式响应和非流式聚合
- 支持 OpenAI `image_url` 图片输入
- 对齐 CodeBuddy CLI 的鉴权、会话和追踪请求头
- 模型列表支持远程发现、缓存和内置列表回退
- 使用可读文本日志，记录提示词、耗时、token 和缓存命中数据
- 运行时不需要 `PROXY_API_KEY`，只需要 `CODEBUDDY_API_KEY`

## 3. 当前思考输出行为

远程 `main`/`v1.0.4` 的实际行为如下：

- `HIDE_REASONING` 未设置或为 `0`：显示 `reasoning_content`
- `HIDE_REASONING=1`：从响应中删除 `reasoning_content`
- 图片请求：将上游思考分片聚合成一个事件（保持单一聚合事件，兼容旧契约）
- 纯文本请求（默认）：**有界窗口合并（coalesce）**——首字符立即出现，并在时间窗口（`REASONING_COALESCE_MS`，默认 250ms）或字节阈值（`REASONING_COALESCE_CHARS`，默认 1024）内把多个上游 `reasoning_content` 分片合并成更少事件发出；既不"等思考结束才发一条"（全量 aggregate 的痛点），也不关闭思考
- 纯文本请求（关闭合并）：`REASONING_COALESCE=0` 时退化为旧版逐片透传
- 当 `HIDE_REASONING` 未设置时，代理**自动注入**默认推理参数：`reasoning_effort: "high"` 与 `reasoning: {"effort":"high","summary":"auto"}`；若客户端已传入其中任一字段，则尊重客户端选择

直连 `https://copilot.tencent.com/v2/chat/completions` 的验证结果：

- 普通请求的 `reasoning_content` 为空，`reasoning_tokens=0`
- 同时传入以下字段后，`hy3` 会返回非空思考：

```json
{
  "reasoning_effort": "high",
  "reasoning": {
    "effort": "high",
    "summary": "auto"
  }
}
```

- 一次完整验证响应包含 15 个思考分片、26 个思考 token，随后正常输出正文、`finish_reason: "stop"`、usage 和 `[DONE]`

## 4. 思考输出方案

### Cherry Studio 思考卡拆分（已解决）

上游对纯文本推理请求会把**同一个推理阶段**拆成几十到几百个独立的 `reasoning_content` 事件（实测单次推理约 224 个分片）。代理若逐片透传，客户端每个分片都会被渲染成一张思考卡。

**当前方案（有界窗口合并 / coalesce，默认开启）**：代理对纯文本请求的 `reasoning_content` 做有界缓冲，在时间窗口或字节阈值内合并成更少事件，首字符延迟与逐片透传基本一致（不等待思考结束）。对于把连续 `reasoning_content` 拼接为单个思考块的客户端（标准 OpenAI 兼容行为），思考卡数量降为 1；对于逐事件渲染的客户端，卡数也从上百降到约几十（可用 `REASONING_COALESCE_MS` 调大以进一步减少）。实测：上游 224 个分片 → 代理约 40 个事件，首字符 ~1.4s，思考文本完整无丢失。

这避免了两条旧路：
- 全量 aggregate：必须等思考结束后才发第一条可见事件，首字符等待从 ~1s 恶化到思考总时长（~15s），体验差；
- 关闭思考（`HIDE_REASONING=1`）：直接不显示思考。

相关实现已合入 `internal/proxy/proxy.go`（`reasoningMode` 枚举：`reasoningOff` / `reasoningAggregate` / `reasoningCoalesce` / `reasoningPassthrough`）。

如确需彻底关闭思考，仍可在部署环境设置：

```yaml
environment:
  HIDE_REASONING: "1"
```

注意：`HIDE_REASONING=1` 会同时**停止自动注入**推理参数并过滤响应中的 `reasoning_content`，从而实现"真正关闭推理"。如果客户端主动传入推理参数，上游仍可能执行推理并产生额外延迟；如需彻底屏蔽，可额外删除客户端传入的 `reasoning` 和 `reasoning_effort`。

### Aggregate / Coalesce 实现注意事项

CodeBuddy 的每个 delta 通常包含：

```json
{
  "function_call": null,
  "tool_calls": [],
  "extra_fields": null
}
```

不能仅根据 `tool_calls` 字段存在就刷新聚合/合并缓冲；空数组 `[]` 不代表发生了工具调用。必须判断数组非空，否则会**退化为每个 token 输出一次思考**（已实现：`requiresReasoningFlush` 仅在 `tool_calls` 为非空数组、或 `content` 非空、或 `finish_reason` 非空时才触发刷新）。

另外，上游会**交替**发送 `reasoning_content` 事件与一个 `content:""` 的空事件；空事件本身无害，但其 `tool_calls:[]` 正是上面这个坑的来源，务必忽略空数组。Coalesce 的刷新触发条件为：时间窗口（`REASONING_COALESCE_MS`）到达、或缓冲字节达到 `REASONING_COALESCE_CHARS`、或出现真正的 `content` / 非空 `tool_calls` / `finish_reason`。

为减少客户端收到的事件噪音，`hasVisibleSSEData` 会跳过只包含空字符串、`null`、空数组或默认角色 `"assistant"` 的 delta，以及 `usage: null` 的占位事件。

## 5. Docker 与发布

`docker-compose.yml` 当前使用：

```yaml
services:
  codebuddycli-proxy:
    image: jobinbai/codebuddycli-proxy-go:latest
    environment:
      CODEBUDDY_API_KEY: "${CODEBUDDY_API_KEY:?Set CODEBUDDY_API_KEY in .env}"
      CODEBUDDY_INTERNET_ENVIRONMENT: "${CODEBUDDY_INTERNET_ENVIRONMENT:-internal}"
      HIDE_REASONING: "1"
    ports:
      - "8787:8787"
    restart: unless-stopped
```

仓库中的实际 compose 文件目前还没有 `HIDE_REASONING`，部署时需要自行添加。

GitHub Actions 在推送 `v*` 标签时自动：

1. 交叉编译各平台可执行文件。
2. 创建 GitHub Release 并上传产物。
3. 使用 `DOCKER_USERNAME` 和 `DOCKER_PASSWORD` 登录 Docker Hub。
4. 构建并推送 `linux/amd64`、`linux/arm64` 镜像。
5. 发布语义化版本标签和 `latest`。

发布示例：

```bash
git tag v1.0.5
git push origin v1.0.5
```

## 6. NAS 防火墙结论

NAS 使用 Docker bridge 网络时，防火墙开启会阻断容器访问 `copilot.tencent.com:443`，关闭防火墙或使用 host 网络则正常。这说明问题位于 NAS 防火墙对 Docker bridge 转发流量的限制，不是应用或 TLS 问题。

已观察到的 Docker 子网包括 `172.17.0.0/16`、多个 `172.x.0.0/16` 和多个 `192.168.x.0/20`。如局域网设备均可信，可按实际安全要求放行 Docker 网段；需要域名解析时还要允许 DNS 53/UDP 和 53/TCP。端口 66–67 是 DHCP 服务端/客户端端口，与访问 CodeBuddy HTTPS 无关。

建议长期做法是固定 compose 网络子网，然后只放行该固定网段所需的出站流量，避免 Docker 每次创建新网络后继续补防火墙规则。

## 7. 常用命令

本地运行：

```bash
export CODEBUDDY_API_KEY='替换为有效密钥'
export CODEBUDDY_INTERNET_ENVIRONMENT=internal
# 默认开启思考与合并；如需关闭思考则取消下一行注释
# export HIDE_REASONING=1
go run ./cmd/codebuddycli-proxy
```

验证：

```bash
go test ./...
go vet ./...
curl http://127.0.0.1:8787/health
```

NAS 部署：

```bash
docker compose pull
docker compose up -d
docker compose logs -f
```

## 8. 安全提醒

本次对话和部分本地粘贴文本中曾出现完整 `CODEBUDDY_API_KEY`。交接文档没有保存该密钥。建议尽快在 CodeBuddy 后台轮换旧密钥，并避免把新密钥写入 Git、命令示例、日志或截图；部署时使用 `.env` 或 NAS 的密钥管理功能。

