# AsterFerry

星槎（AsterFerry）是面向私有网络的轻量级中继：公网 `gateway` 与内网
`agent` 通过 TLS 1.3 + QUIC 连接，提供本地 SOCKS5/HTTP 代理和 TCP/UDP
反向映射。

当前配置协议为 v2，使用严格的角色分区：

- `gateway`：公网入口、Agent mTLS/token 认证、反向端口映射和受策略约束的出口代理。
- `agent`：主动连接 Gateway，提供本地代理并把内网服务注册为反向映射。

```text
asterferry gateway -c gateway.yaml
asterferry agent   -c agent.yaml
asterferry validate -c agent.yaml
```

## 流量模型

```text
本地应用 -> agent.proxy -> direct 或 gateway egress -> 目标地址
公网用户 -> gateway_port -> agent.reverse[].local
```

代理数据在 QUIC 加密流内使用 AsterFerry relay record 传输；`balanced` profile
使用有界随机 padding 降低固定长度指纹，但不伪装成 HTTP/3、WebSocket 或其他业务协议。

## 配置示例

完整的可复制模板见 [examples/README.md](examples/README.md)、
[examples/gateway.yaml](examples/gateway.yaml) 和
[examples/agent.yaml](examples/agent.yaml)。生产部署前必须替换证书、客户端 CA、token、
ALPN 部署标识和所有示例地址。

Gateway 防火墙需要放行 QUIC 使用的 UDP 端口，以及实际配置的反向 TCP/UDP 端口。管理接口
默认只监听 loopback：Gateway 为 `127.0.0.1:9090`，Agent 为 `127.0.0.1:9091`。

## 安全边界

- 生产默认要求 Gateway 服务器证书、Agent 客户端证书和 per-agent token。
- Gateway 出口代理默认按 Agent、协议、端口和目标 IP 执行 ACL，并拒绝私网、loopback、链路本地和 metadata 地址。
- 管理接口不应通过公网暴露。
- 日志不记录 token、私钥或代理 payload。

## 运行检查

```powershell
asterferry validate -c examples/gateway.yaml
asterferry validate -c examples/agent.yaml
go test ./...
go vet ./...
```

历史 `myproxy` 和 `myfrp` 仅作为设计来源，不兼容 AsterFerry v2 线协议。
