# AsterFerry 运行时观测与运维

本指南说明统一 Node 模型下的运行时运维能力。每台主机仍只运行一个
`asterferry node run` 进程；Gateway 和 Agent 是下发到 Node 的行为规格，
运行时运维不需要重启进程。

## 默认可见内容

Node 建立经过认证的 Controller 控制流后，会上报不含载荷的以下元数据：

- Node 到 Node 的 AFDP 会话；
- 承载 Service 的 TCP 连接和 UDP flow；
- Agent 到 Gateway 的 egress 流。

元数据包括连接 ID、类型、状态、进程可观察到的来源 IP/端口、对端 Node、
Gateway/Agent、Assignment、Service、目标地址、协议、开始/最后活动/结束
时间、累计字节和自连接开始计算的累计平均字节速率。不会包含业务载荷、数据包内容、凭证或完整
请求数据。

Node 本地使用有上限的内存 registry 保存活动连接。Controller 保存当前/最近
连接视图、生命周期事件和分钟级流量汇总，历史保留 30 天。重连或进程重启
可能丢失临时操作控制，但不会把已经上报的已关闭连接历史重新变成活动状态。

打开某个 Node，进入“观测”即可查看默认只读表。Gateway 行会展示 Agent 对端；
反向 Service 还会展示进入 Gateway 的客户端来源 IP。Agent 行会展示 Gateway
对端和本地目标。

## 高级运维操作

高级操作默认关闭。在“Dashboard → 管理 → 高级运行时运维”由 Admin 打开。开关
关闭时，基础元数据仍然可见。

打开后，Operator 可以在“Node → 观测”对活动连接执行：

- **断开**：关闭选中的会话、TCP 连接、UDP flow 或 egress 流；
- **限速**：按字节/秒设置临时入站、出站或双向限速，并设置 TTL（最长 24 小时）；
- **清限速**：移除临时限速。

控制保存在 Node 进程内，不属于期望配置快照，因此替换或重启后不会被静默
恢复。关闭开关会阻止新的操作请求，并请求已连接 Node 清除限速；关闭期间
重新连接的 Node 也会收到清理指令。

方向中的 `in`/`out` 以 Node 为参照，表示流量进入/离开该 Node：Gateway 反向
Service 中，客户端到 Agent 是入站，Agent 到客户端是出站；Gateway egress
中，Agent 到外部目标是入站，外部目标到 Agent 是出站。每个请求都会写审计；
投递是尽力而为，离线 Node 不会自动收到旧的运维操作。

## Controller HTTP 端点策略

端点策略是有意设计的：`/healthz` 完全匿名；`/readyz` 和 `/metrics` 需要
经过认证的 Viewer、Operator 或 Admin；`/openapi.yaml` 及
`/api/v1/openapi.yaml` 为便于客户端发现而保持匿名。如果部署环境不应公开
API 元数据，请在 HTTPS ingress 或网络策略层限制 OpenAPI 路径。Prometheus
应使用只读 Viewer API Token。

浏览器 Cookie 会话只保存在 Controller 进程内存中，有效期 12 小时；进程重启
或请求落到另一副本都会使其失效，自动化客户端应使用 API Token。共享会话存储
和 Controller HA 不在本版本范围内。

## REST API

以下资源路径均位于 `/api/v1` 下，并需要对应 Controller 角色：

| 路径 | 角色 | 用途 |
| --- | --- | --- |
| `GET /runtime/settings` | Viewer | 查询开关和保留期限 |
| `PUT /runtime/settings` | Admin | 开启或关闭高级操作 |
| `GET /runtime/connections` | Viewer | 全局查询当前/最近连接元数据 |
| `GET /nodes/{id}/runtime/connections` | Viewer | 查询单个 Node 的连接元数据 |
| `GET /runtime/events` | Viewer | 查询保留的生命周期/操作事件 |
| `GET /runtime/traffic` | Viewer | 查询分钟级流量汇总 |
| `GET /runtime/stream` | Viewer | 接收 SSE 变化通知 |
| `POST /nodes/{id}/runtime/actions` | Operator | 按筛选器执行操作 |
| `POST /nodes/{id}/runtime/connections/{connection}/actions` | Operator | 操作单条连接 |

全局和 Node 连接查询支持 `state`、`type`、`source_ip`、`peer_node_id`、
`gateway_id`、`agent_id`、`assignment_id`、`service_id`、`protocol` 和
`limit`。按筛选器操作支持 `connection_id`、`source_ip`、`peer_node_id`、
`assignment_id`、`service_id` 和 `protocol`。Node 级操作必须提供筛选器，
空筛选器不会误操作所有连接。

例如，Admin 已打开开关后，把某个来源在十分钟内的 TCP 连接限制为 1 MiB/s：

```sh
curl -X POST https://controller.example/api/v1/nodes/gateway-b/runtime/actions \
  -H 'Authorization: Bearer <operator-token>' \
  -H 'Content-Type: application/json' \
  -d '{
    "action": "rate_limit",
    "selector": {"source_ip": "198.51.100.24", "protocol": "tcp"},
    "direction": "both",
    "bytes_per_second": 1048576,
    "burst_bytes": 2097152,
    "ttl_seconds": 600
  }'
```

返回值只表示请求已接受/排队，或已投递到当前控制流；不保证 Node 执行时连接
仍然存在。应结合运行时事件流和 Node“观测”页面确认结果。

## Controller 数据库运维

小规模、单副本 Controller 默认使用 SQLite。随着 Node 数量或运行时事件量
增加，建议使用带有有界连接池的 PostgreSQL。初始化时通过
`--database-driver postgres --database-url 'postgres://...'` 选择它。

当前开发 schema 是破坏性契约，不提供 `controller migrate` 或
SQLite→PostgreSQL 原地转换。需要切换后端或 schema 时，请重新初始化
Controller，并在 Dashboard 重建资源；v8/v9 数据库和 v10 之前的备份
manifest 会被拒绝。PostgreSQL 备份/恢复要求执行 CLI 的机器安装
`pg_dump` 和 `pg_restore`；备份还包含 Controller 配置、master key、CA 和
TLS 身份。

## 故障排查

- 没有连接行：确认 Node 证书 active、控制流已认证，并检查 Node 日志中是否
  启用了 `runtime-telemetry-v1`。
- 行状态为 stale/unknown：Node 离线或最近快照尚未到达。当前状态采用安全
  语义，不能从 Controller 的期望配置推断活动连接。
- 返回 `advanced_operations_disabled`：先由 Admin 打开开关；即使已打开，
  Viewer 也不能执行操作。
- 请求已接受但状态没变化：Node 可能离线，连接可能在查询和执行之间结束，
  或操作通道已满。检查运行时操作结果事件，确认筛选器后再有意重试。
- 限速消失：TTL 到期、Node 重启、generation 替换，或 Admin 关闭了功能。
  运行时控制不是配置状态。

Controller 不进入业务数据路径。如果需要按载荷检查，应使用应用协议自身的
日志和 tracing；本功能刻意只保留元数据。
