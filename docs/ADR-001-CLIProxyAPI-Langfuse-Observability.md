# ADR-001：CLIProxyAPI 统一接入 OpenTelemetry 与 Langfuse

- 状态：架构决策已采纳；各能力的集成状态见下文。
- 决策日期：2026-09-24；实施状态核对：2026-09-29。
- 范围：CLIProxyAPI 的模型请求观测；Langfuse 是观测后端，不是请求代理。

## 背景与决策

Claude Code、Codex 等客户端共用 CLIProxyAPI 转发模型请求。若分别在客户端植入 Langfuse，接入与维护会随客户端增加，且不能统一记录代理内部的路由、供应商结果和用量。决定在 CLIProxyAPI 接收请求时建立 OpenTelemetry（OTEL）trace，经 OTLP/HTTP 异步导出至 Langfuse；模型流量仍由 CLIProxyAPI 直接转发。Langfuse 故障不得阻断模型调用。

不采用 Langfuse 作为模型流量的中间代理，也不在 CLIProxyAPI 内调用其私有 ingestion API。OTEL 的导出边界允许日后接入其他兼容后端；Collector 和客户端工具链级 trace 不属于当前实现。代理侧仅能记录其实际可见的请求、模型尝试与用量，不能凭此还原客户端的完整工具调用树。

## 已有实现：OTLP 导出

以下映射以 `origin/dev` 的 `5213ede84968419a09ce89131d478d99b1ab6da5` 为准；该分支已包含 OpenTelemetry 主链，但 `origin/main` 的 `c404af96ebacedf8168b3c2bdbf4449a21cd1c1e` 尚不包含此模块。这里的「已实现」不表示已经部署到生产环境。

| 阶段 | 代码入口 | 当前行为 |
| --- | --- | --- |
| 配置与生命周期 | [配置类型](../internal/config/config_types.go)、[默认值](../internal/config/config_defaults.go)、[服务生命周期](../sdk/cliproxy/service_lifecycle.go) | 默认关闭；启动时初始化，停止时限时 flush；配置无效时禁用观测，不阻断服务启动。 |
| HTTP 根 span | [路由注册](../internal/api/server.go)、[中间件](../internal/observability/middleware.go) | 对已启用的请求创建根 span，记录路由、HTTP 状态、耗时和既有 request ID；通过请求 context 向下游传递 trace。 |
| 模型 generation span | [用量上报](../internal/runtime/executor/helps/usage_helpers.go)、[用量插件](../internal/observability/usage_plugin.go) | 复用 usage 发布链，对每次完成的模型尝试创建根 span 的子 span，记录 provider、requested/resolved model、stream、可获得的 token、延迟和错误；不在每个 executor 单独植入 OTEL。 |
| OTLP/HTTP | [Provider](../internal/observability/provider.go)、[配置解析](../internal/observability/config.go) | BatchSpanProcessor 使用有界队列和批量导出；认证从环境变量读取，exporter 使用 Basic Auth。导出失败或队列满时不回压模型转发。 |
| 可选 payload 捕获 | [捕获](../internal/observability/capture.go)、[脱敏](../internal/observability/redact.go) | 输入/输出默认不捕获；显式启用时截断并脱敏后附于 HTTP span。脱敏不能代替对实际请求内容的隐私审查。 |

HTTP 根 span 和 generation span 属于同一个 trace；generation span 在 usage 发布时结束，不额外创建 `request.parse`、`route.resolve` 或每个 token/chunk 的 span。Streaming 的最终上报依赖已有 usage 生命周期。Trace ID 与代理既有 request ID 是不同标识，后者只作为 `cpa.request_id` 属性关联。

### 配置与故障边界

配置字段以 [公开模板](../config.example.yaml) 为准：`observability.enabled` 默认 `false`；`service-name` 配置 OTEL resource 的 `service.name`；`exporter.endpoint` 必须是完整的 OTLP/HTTP traces URL（例如 `https://<host>/api/public/otel/v1/traces`），exporter 不会自动追加 `/v1/traces`。`public-key-env` / `secret-key-env` 默认指向 `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` 环境变量；配置文件只记录变量名，不存储值。`exporter.insecure` 仅供明文 HTTP 开发端点使用。

配置可调的上报超时、队列大小、批量大小、批处理间隔和退出 flush 超时见模板。关闭或配置无效时不创建 exporter、span 或后台工作；上报排队和导出不处于模型请求的同步必需链路。`capture.input` / `capture.output` 默认关闭，开启时由 `capture.max-bytes` 限制记录长度。不要在配置、ADR、测试输出或故障报告中记录真实凭据、请求原文和用户标识。

## Sessions / Users：已交付功能分支，尚未进入 dev

在本次核对基线，`origin/dev` 的 [usage 插件](../internal/observability/usage_plugin.go) 尚未上报 `session.id`、`user.id`，也没有 `internal/observability/identity.go`；因此不能把 Langfuse Sessions/Users 写成已经在 `dev` 或 `main` 生效。功能实现已提交在远端 `feat/ruyi-217-langfuse-upstream`，核对 tip 为 `90d013ea94099cc879a80a7fc7acb1f53abb6a7f`，核心会话提交为 `4b997146df1ab4542d7dec27eaac88002399b673`、同步修正为 `ff3e88a5`。该分支与 `dev` 当前分叉，集成前须重新核对代码与文档对应关系。

| 维度 | 功能分支实现 | 含义 |
| --- | --- | --- |
| 会话来源与规范化 | [`identity.go`（功能分支）](https://github.com/cfgxy/CLIProxyAPI/blob/90d013ea94099cc879a80a7fc7acb1f53abb6a7f/internal/observability/identity.go) 复用 `sdk/cliproxy/session` | 已识别的会话归一为稳定 UUID；`session.id` 仅在识别到会话时输出。路由实际选用的会话优先于入口初值；父会话、客户端类型、Agent 名称等附加维度使用 `cpa.*` 属性。 |
| HTTP 根 span | [`middleware.go`（功能分支）](https://github.com/cfgxy/CLIProxyAPI/blob/90d013ea94099cc879a80a7fc7acb1f53abb6a7f/internal/observability/middleware.go) | 请求内的身份 holder 汇合入口识别、认证调用方作用域与执行路径同步回传的实际会话，根 span 结束前设置属性。 |
| generation span | [`usage_plugin.go`（功能分支）](https://github.com/cfgxy/CLIProxyAPI/blob/90d013ea94099cc879a80a7fc7acb1f53abb6a7f/internal/observability/usage_plugin.go) | 从同一请求身份 holder 与 `usage.Record.SessionID` 得出身份，确保路由后的会话优先；写 `session.id` 和非空 `user.id`。 |
| 隐私开关 | [`config.example.yaml`（功能分支）](https://github.com/cfgxy/CLIProxyAPI/blob/90d013ea94099cc879a80a7fc7acb1f53abb6a7f/config.example.yaml) | `observability.identity.plaintext-user-id` 默认 `false`；显式启用才允许输出原始显式 ID / 客户端账号 ID。 |

`user.id` 的优先级为显式用户标识头、客户端内建账号标识、调用方作用域、固定 `anonymous`。前两级默认通过调用方作用域摘要处理，第三级本身已经是不可逆摘要；`plaintext-user-id` 不改变后两级。匿名兜底保证 `user.id` 不为空，但无法把匿名请求区别为不同真实用户。`session.id` 缺失时不会虚构会话，因此 Sessions 页面是否出现记录仍取决于实际请求身份信息。功能分支的 `identity_test.go` 和 `identity_span_test.go` 覆盖回退链、哈希/明文切换与根/generation span 的一致性；这些测试不替代集成后的重新验证。

## 验收与后续边界

- 静态检查：在目标集成分支确认配置字段、两个 span 的父子关系、用量和身份属性均存在；不能用功能分支的代码代替已集成证据。
- 本地验证：使用 `go test ./internal/observability/...` 和 `go build -o test-output ./cmd/server` 检查目标源码；对 Sessions/Users 的验证必须使用包含其实现的基线。
- 运行时验证：在获授权的开发或 QA 环境检查 OTLP/HTTP traces 是否被接收、断连时请求仍可用，以及 Sessions/Users 按请求身份分组；生产部署与服务状态不由本 ADR 推定。
- 回退：关闭 `observability.enabled` 即停止新 span/exporter；配置或 exporter 异常不得影响模型转发。历史已导出的记录须由观测后端独立管理。

本记录只描述 CLIProxyAPI 的代理侧决定及各分支实际状态；不授权修改 Langfuse 部署、客户端会话头或运行时配置。
