# AsterFerry

AsterFerry 是一个自托管的内网穿透和流量转发系统。Controller 负责身份、
权限、配置、调度和审计；每台数据面主机运行一个通用 Node 进程，Controller
通过下发规格将 Node 配置为 Gateway 或 Agent。

```text
Dashboard / CLI -- HTTPS --> Controller -- mTLS gRPC --> Node
                                      |
                                      +-- SQLite（默认）或 PostgreSQL

Gateway <========== AFDP/2 over QUIC ==========> Agent
```

本文只介绍 Linux 部署，并提供两种方式：源码构建和 GitHub Release 一键安装。

## 端口和前置条件

Controller 默认使用以下端口：

| 端口 | 用途 |
| --- | --- |
| TCP 8443 | HTTPS API 和 Dashboard |
| TCP 9443 | Node 连接 Controller 的 mTLS gRPC |
| UDP 4433 | Gateway 默认 AFDP/2 数据端口 |
| TCP 9090 | 本机 Prometheus 指标，默认只监听 127.0.0.1 |

`--http-listen`、`--grpc-listen` 可以绑定 `0.0.0.0`，但
`--grpc-advertise` 必须填写 Node 实际可以访问的 IP 或域名，不能填写
`0.0.0.0`。请同时在云安全组和主机防火墙放行实际需要的端口。

源码构建需要 Go 1.26.7、Node.js 24.19.0 和 npm 12.0.2；Node.js 22.12.0
及 npm 11 也属于兼容工具链。具体版本见 [`.toolchain.json`](.toolchain.json)。

## 方式一：下载源码、构建并启动

### 1. 下载并构建 Linux amd64

```bash
git clone https://github.com/eternallyzzz/asterferry.git
cd asterferry

npm --prefix web/dashboard ci \
  --audit=false \
  --registry=https://registry.npmjs.org \
  --replace-registry-host=always
npm --prefix web/dashboard run build

mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -tags=dashboard_assets -trimpath \
  -ldflags="-s -w" \
  -o dist/asterferry-linux-amd64 ./cmd/asterferry
```

上面的 `dashboard_assets` 会把 Dashboard 编进单个二进制。构建 ARM64 时将
`GOARCH=amd64` 和输出文件名改成 `arm64`。

### 2. 初始化并启动 Controller

在 Controller 主机执行。`--grpc-advertise` 换成这台主机的公网 IP、内网 IP
或 DNS 名称：

```bash
if ! id asterferry >/dev/null 2>&1; then
  sudo useradd --system --home-dir /var/lib/asterferry \
    --shell /usr/sbin/nologin asterferry
fi
sudo install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
sudo install -m 0755 dist/asterferry-linux-amd64 /usr/local/bin/asterferry

sudo -u asterferry /usr/local/bin/asterferry controller init \
  --dir /var/lib/asterferry \
  --http-listen 0.0.0.0:8443 \
  --grpc-listen 0.0.0.0:9443 \
  --grpc-advertise controller.example.com:9443

sudo install -m 0644 deploy/asterferry-controller.service \
  /etc/systemd/system/asterferry-controller.service
sudo systemctl daemon-reload
sudo systemctl enable --now asterferry-controller.service
```

首次初始化会生成 Admin 密码并只显示一次，请立即保存。然后访问
`https://controller.example.com:8443/dashboard/`。

### 3. 构建并启动通用 Node

在每台 Node 主机安装同一个二进制，并从 Controller 安全地复制
`ca/ca.crt`。先在 Dashboard 创建一个未配置角色的 Node 记录，再在 Controller
上为它生成一次性 Token。B、C 两台机器使用完全相同的注册流程：

```bash
sudo -u asterferry /usr/local/bin/asterferry enroll-token create \
  --config /var/lib/asterferry/controller.json \
  --node-id <node-id>
```

在 Node 主机执行 enroll。`<token>` 使用上一步输出的 Token，CA 文件只需要
Controller 的 `ca.crt`，不要复制 `ca.key`：

```bash
if ! id asterferry >/dev/null 2>&1; then
  sudo useradd --system --home-dir /var/lib/asterferry \
    --shell /usr/sbin/nologin asterferry
fi
sudo install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
sudo install -m 0755 dist/asterferry-linux-amd64 /usr/local/bin/asterferry
sudo install -o asterferry -g asterferry -m 0644 controller-ca.crt \
  /var/lib/asterferry/controller-ca.crt

sudo -u asterferry /usr/local/bin/asterferry node enroll \
  --controller controller.example.com:9443 \
  --token '<token>' \
  --node-id <node-id> \
  --ca /var/lib/asterferry/controller-ca.crt \
  --output /var/lib/asterferry/node-bootstrap.json \
  --cache /var/lib/asterferry/snapshot.cache

sudo install -m 0644 deploy/asterferry-node.service \
  /etc/systemd/system/asterferry-node.service
sudo systemctl daemon-reload
sudo systemctl enable --now asterferry-node.service
```

注册成功后，Node 会以“未配置行为”状态在线等待 Controller 下发规格。
角色不是二进制参数，也不是两个不同的程序；在 Dashboard 的 Node 详情中
选择 Gateway 或 Agent 并保存后，Controller 才会下发对应行为。Gateway 还
需要配置可访问的 AFDP/2 公网端点和端口池，Agent 需要选择已注册的 Gateway
并配置路由或出口策略。

### A/B/C 示例：先注册，再配置角色

假设 A 是 Controller，公网 IP 为 `47.98.144.86`；B 是内网机器，最终承担
Agent；C 是公网机器，最终承担 Gateway：

1. A 按上面的 Controller 步骤初始化，`--http-listen` 和 `--grpc-listen`
   绑定 `0.0.0.0`，`--grpc-advertise` 填 `47.98.144.86:9443`。安全组放行
   TCP `8443`、`9443`。
2. 在 Dashboard 创建 `node-b` 和 `node-c` 两个通用 Node，分别生成一次性
   安装命令。
3. B 执行通用 Node 注册命令。B 只需要能主动访问 A 的 TCP `9443`，安装时
   不选择角色。
4. C 执行同一个通用 Node 注册命令。C 需要能主动访问 A 的 TCP `9443`，并
   按 Gateway 规格放行对外的 UDP `4433`（或你配置的 AFDP/2 端口）。
5. 等 C、B 都显示为已注册后，先打开 C 的 Node 详情，选择 **Gateway**，填写
   C 的公网 AFDP/2 端点并保存。
6. 再打开 B 的 Node 详情，选择 **Agent**，从“绑定 Gateway”下拉框选择
   `node-c` 并保存。该绑定是固定目标；C 不可用时不会自动切换到其他 Gateway。

Controller 只负责身份、配置和调度，B 与 C 的业务流量通过 AFDP/2 直接传输。

## 方式二：一条命令安装最新 Release

这种方式适用于带 systemd 的 Linux。命令从 GitHub 的 `main` 分支下载入口
脚本；脚本通过 GitHub Releases API 查找最新的语义化 tag（包括 RC），下载
对应架构的压缩包和 `SHA256SUMS`，校验成功后安装并启动服务。

### 安装 Controller

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | sudo bash -s -- \
      --grpc-advertise controller.example.com:9443
```

脚本默认监听 `0.0.0.0:8443` 和 `0.0.0.0:9443`，使用
`/var/lib/asterferry`，创建 `asterferry-controller.service` 并自动启动。
首次安装时生成的 Admin 密码会在安装输出中显示一次。

需要固定版本时：

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | sudo bash -s -- \
      --grpc-advertise controller.example.com:9443 \
      --version 1.0.0-rc.1
```

### 安装通用 Node（注册后配置角色）

先登录 Dashboard，在 **Nodes** 页面创建安装任务，取得 Node ID、一次性 Token
和 Controller CA。B、C 都执行这条通用命令，安装时不传角色参数；Dashboard
生成的完整命令可以直接执行。下面是使用安装脚本、自动解析最新 Release 的
Linux 命令模板：

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-node.sh \
  | sudo bash -s -- \
      --node-id <node-id> \
      --controller controller.example.com:9443 \
      --token '<one-time-token>' \
      --ca-pem-b64 '<base64-controller-ca>' \
      --arch amd64
```

不传 `--version` 时，Node 安装脚本会自动下载最新二进制、校验 checksum、
完成通用 Node enroll、创建 `asterferry-node.service` 并自动启动。Node 注册后，
回到 Dashboard 配置 Gateway 或 Agent；Agent 配置时选择已注册的 Gateway。
Token 只使用一次且短时间有效，不要把完整命令发送到公开聊天、工单或日志中。

安装脚本常用参数：

```text
--version VERSION       固定版本，默认自动选择最新 tag
--arch amd64|arm64      覆盖自动架构识别
--data-dir DIR          修改状态目录
--service-name NAME     修改 Node 的 systemd 服务名
--repo OWNER/REPO       使用 GitHub fork
```

使用私有 HTTPS 发布源时，必须同时提供 `--release-base-url` 和
`--version`；默认 GitHub 源不需要这两个参数。

### 验证服务

```bash
sudo systemctl status asterferry-controller.service
sudo systemctl status asterferry-node.service
sudo journalctl -u asterferry-controller.service -f
sudo journalctl -u asterferry-node.service -f
curl -k https://controller.example.com:8443/healthz
```

## 备份与参考文档

Controller 的备份应包含数据库、CA、TLS 身份和 master key：

```bash
sudo -u asterferry /usr/local/bin/asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

- [架构与契约](docs/architecture.md)
- [中文运维指南](docs/operations.zh-CN.md)
- [兼容性说明](docs/compatibility.md)
- [支持矩阵](docs/support-matrix.md)
- [GeoIP 配置](docs/geoip.md)
- [发布流程](docs/release-runbook.md)
- [安全策略](SECURITY.md)
