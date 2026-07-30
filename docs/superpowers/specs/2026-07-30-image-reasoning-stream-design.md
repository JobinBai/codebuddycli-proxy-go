# 图片请求的思考流兼容设计

## 目标

默认保留模型思考过程，同时避免 Cherry Studio 将图片请求中上游逐片的 `reasoning_content` 渲染为大量独立思考卡。

## 行为

- `HIDE_REASONING` 未设或为 `0`：不含 `image_url` 的流式请求逐片透传 `reasoning_content`，保留实时思考展示；含 `image_url` 的流式请求则将连续思考累积后作为单个事件发送。
- `HIDE_REASONING=1`：两类请求都不发送思考过程。
- 非流式请求：沿用现有聚合行为；仅在启用思考时返回完整的聚合 `reasoning_content`。

## 实现边界

图片判断仅检测 OpenAI `messages[].content[]` 中的 `type: "image_url"`。聚合缓冲区设置上限，超出上限时仍维持单条聚合输出而不记录或无限制占用内存。文本 `content`、工具调用、usage、结束原因与 `[DONE]` 事件保持原有顺序和语义。

## 测试

覆盖三种流：默认隐藏、纯文本逐片透传、图片请求聚合为单条思考事件并继续输出最终答案。测试还验证 usage 与终止事件不丢失。
