# AsterFerry v2 配置示例

配置使用两个固定角色：`gateway`（公网入口）和 `agent`（内网节点）。
`tunnels` 已统一命名为 `reverse`，公网端口字段为 `gateway_port`。

## 1. 生成部署标识

每个部署生成一个只在 Gateway 和 Agent 配置中使用的 ALPN 标识：

```powershell
$alpn = -join ((48..57) + (97..122) | Get-Random -Count 16 | ForEach-Object {[char]$_})
$alpn
```

将它写入两端 `transport.alpn`。不要直接使用仓库示例值。

## 2. 准备 TLS 证书

Gateway 使用服务器证书和 Agent 客户端 CA；Agent 使用自己的客户端证书。测试环境可以先使用自签名 CA，生产环境应使用内部 CA 或正式 PKI。

Agent 的服务器名称必须匹配 Gateway 证书 SAN。证书和私钥应限制为服务用户可读。

## 3. 创建 Agent token

```powershell
openssl rand -hex 32 | Set-Content -NoNewline edge-a.token
```

Gateway 和 Agent 两端必须使用同一个 token；每个 Agent 使用独立 token。不要把 token、私钥或真实证书提交到仓库。

## 4. 修改配置

- Gateway：`gateway.listen`、TLS 文件、`gateway.agents[].token_file`
- Gateway：为每个 Agent 配置独立的 `reverse` ACL 和 `egress` ACL
- Agent：`agent.server`、`agent.tls.*`、`agent.token_file`
- Agent：`agent.reverse[].local` 指向内网服务
- Agent：本地代理使用 `agent.proxy.inbounds`；默认出口为 Gateway

代理默认只监听 loopback。若绑定到其他地址，必须配置用户名和密码。

## 5. 校验和启动

```powershell
asterferry validate -c examples/gateway.yaml
asterferry validate -c examples/agent.yaml
asterferry gateway -c examples/gateway.yaml
asterferry agent -c examples/agent.yaml
```

Gateway 防火墙至少放行 UDP `4433`，以及实际使用的反向 TCP/UDP 端口。管理端点默认只监听 loopback。
