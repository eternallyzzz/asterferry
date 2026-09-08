# AsterFerry 中文说明

[English README](../README.md)

AsterFerry 是一个自托管的私有网络转发系统。Controller 负责身份、访问
控制、配置和调度；每台数据面主机只运行一个通用 Node，注册后由
Controller 为它分配 Gateway 或 Agent 行为。

```text
Dashboard / CLI -- HTTPS --> Controller -- mTLS gRPC --> Node
                                      |
                                      +-- SQLite 或 PostgreSQL

Gateway <========== AFDP/1 over QUIC ==========> Agent
```

wire 协议是 AFDP/1 和 control/1，数据库 schema v1。

## 三分钟 Linux 快速开始

准备 Linux Controller 主机、Node 可访问的 Controller 地址，以及一台或多台
Linux/Windows Node 主机。安装器会创建服务、证书、数据库和第一个 Admin 账户。

把 `<CONTROLLER_IP>` 设为所有 Node 都能访问的地址。请以 root 运行；非 root
shell 把命令最后的 `bash` 换成 `sudo bash`。

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | bash -s -- --grpc-advertise <CONTROLLER_IP>:9443
```

1. 打开 `https://<CONTROLLER_IP>:8443/dashboard/`，保存安装器只显示一次
   的 Admin 初始密码。
2. 进入 **Nodes** 创建安装任务，在目标主机执行生成的命令。Linux 命令以
   root 或 `sudo` 执行，Windows 命令在管理员 PowerShell 中执行。不要修改
   生成的 Node ID 或 enrollment token。
3. 在 Node 详情中给一台 Node 保存 **Gateway** 规格，给另一台保存
   **Agent** 规格，并把 Agent 绑定到该 Gateway。
4. 创建 **Service**，配置公网入口和本地目标，然后通过 Gateway 入口验证
   流量。

需要可复现部署时，请使用指定 GitHub Release 中对应的 installer 资产并固定版本。

## 网络与部署

只开放当前拓扑需要的端口，并用主机防火墙或网络策略限制来源。

| 端口 | 方向 | 用途 |
| --- | --- | --- |
| TCP 8443 | 客户端 → Controller | HTTPS API 和 Dashboard |
| TCP 9443 | Node → Controller | mTLS 控制连接 |
| UDP 4433 | 客户端 ↔ Gateway | 默认 AFDP/1 数据端口 |
| TCP 9090 | Prometheus → Controller | 可选，默认仅监听回环地址的 metrics |

Controller 可以监听 `0.0.0.0`，但 `--grpc-advertise` 必须是 Node 能实际
访问的地址，不能填写 `0.0.0.0`。

### Windows 与 WSL

从指定 Release 下载 `install-controller.ps1`，在管理员 PowerShell 中运行：

```powershell
.\install-controller.ps1 -GrpcAdvertise <CONTROLLER_IP>:9443
```

Dashboard 生成的 Node 命令支持发布的 Linux 和 Windows Node 资产。WSL2
支持 systemd 和无 systemd fallback，但只是兼容性测试环境，不是正式支持目标。

### Container 与 Helm

Container 和 Helm 部署使用运维方构建的镜像。初始化 Controller 数据目录，
然后在
[`deploy/docker/compose.yaml`](../deploy/docker/compose.yaml) 或
[`Controller chart`](../deploy/helm/asterferry-controller) 和
[`Node chart`](../deploy/helm/asterferry-node) 中显式设置
`image.repository`。升级时替换镜像；不要在运行中的容器内替换二进制。

SQLite 只支持单个 Controller 副本。PostgreSQL 支持通过外部 readiness-aware
负载均衡器或 Kubernetes Service 运行 active/standby 双副本；两个副本必须
共享 Controller identity、master key 和数据库。

## 日常运维

### 健康检查与日志

原生 Linux 服务可以这样检查：

```bash
sudo systemctl status asterferry-controller.service
sudo journalctl -u asterferry-controller.service -f
curl --fail --insecure https://127.0.0.1:8443/healthz
curl --fail --insecure https://127.0.0.1:8443/readyz
```

`/healthz` 检查进程；`/readyz` 是外部路由使用的就绪信号。管理 HTTPS 上的
`/metrics` 需要认证；独立 metrics listener 默认只监听回环地址，只应暴露给
可信 Prometheus 网络。

### 备份与恢复

把数据库、Controller 配置、CA、TLS identity 和 master key 一起备份。原生
安装可以使用：

```bash
sudo -u asterferry /var/lib/asterferry/bin/asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

先在临时恢复目录验证备份。PostgreSQL 备份使用 `pg_dump` 和 `pg_restore`。
恢复备份会使浏览器 session 失效，并重置 Controller lease。

### 升级与退役

- 升级前完成并验证 Controller 备份。
- 当 wire 或数据库契约变化时，Controller 和 Node 应使用同一 release line。
- 原生自更新使用带签名的 release manifest，校验归档摘要，就绪检查失败时回滚。
- Container 和 Helm 部署必须滚动到新的审核镜像。
- 不再允许认证的 Node 使用 **Retire**；Service 和审计历史会保留，只有在
  没有依赖后才能永久删除。

## 故障排查

| 现象 | 检查 |
| --- | --- |
| Dashboard 打不开 | 检查 Controller 服务、TCP 8443 和 HTTPS listen 地址。 |
| Node 一直 pending | 检查 `--grpc-advertise` 是否可解析、Node 是否能访问 TCP 9443；enrollment token 会过期且只能使用一次。 |
| Node 已注册但没有流量 | 保存 Gateway/Agent 规格，把 Agent 绑定到 Gateway，创建 Service，并检查 Gateway 防火墙和公网入口。 |
| 流量被拒绝 | 检查 Service 目标、协议/端口、assignment 和 Node 的 **Observed** 页面。 |
| root 安装提示没有 `sudo` | root 直接执行最后的安装命令；`sudo` 只供非 root shell 使用。 |

## 安装安全提醒

- Admin 初始密码、enrollment token 和生成的 bootstrap 文件都要当作密钥，
  不要发到 issue、聊天或 shell history。
- 不要复制或暴露 Controller CA 私钥。Controller 数据目录、数据库、TLS
  identity 和 master key 需要同等保护。
- 使用 HTTPS 的发布地址，不要修改 Dashboard 生成的 enrollment 命令；需要的
  Controller/Node 端口应放在防火墙之后。
- 原生升级器只在本机 readiness 探测中跳过证书校验；Release manifest 会先验签，
  下载的资产还会按已签名的 SHA-256 摘要校验。
- 不要把可选 metrics listener 暴露到公网；使用认证的管理 metrics 或可信的
  内部抓取网络。

## 支持范围与接口

| 范围 | 状态 |
| --- | --- |
| 原生 Controller/Node | Linux amd64/arm64、Windows amd64 |
| WSL2 | 兼容性测试，不是正式支持目标 |
| 数据库 | 单副本使用 SQLite；生产规模和 active/standby 双副本使用 PostgreSQL |
| Container/Helm | 支持自建镜像和仓库内 chart |
| GeoIP 路由 | 可选的外部、经过审核的 MaxMind-compatible 数据库，不随项目打包 |

Controller 在 `/openapi.yaml` 和 `/api/v1/openapi.yaml` 提供 OpenAPI。
源文件是 `internal/controller/openapi.yaml`，
[`api/openapi.yaml`](../api/openapi.yaml) 是生成副本。wire schema 位于
[`control.proto`](../proto/controlwire/v1/control.proto) 和
[`data.proto`](../proto/afdp/v1/data.proto)。
