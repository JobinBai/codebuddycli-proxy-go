# Go 代理设计

服务以 Go 标准库实现当前 JavaScript 主分支的直连模式：接收 OpenAI Chat Completions 请求、以 CLI 兼容请求头调用 CodeBuddy、将 SSE 直接转发或聚合为非流式响应。会话存储仅保存在内存中，并按 TTL 回收。

运行产物是无 CGO 的 `codebuddycli-proxy`。Docker 采用 Go Alpine 构建器与 scratch 运行时，复制 CA 根证书以支持 HTTPS。GitHub Actions 在主分支测试；推送版本标签时交叉构建 Linux/macOS/Windows 的 amd64/arm64 资产、创建 Release，并构建发布 amd64/arm64 GHCR 镜像。
