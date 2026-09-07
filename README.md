# AsterFerry

AsterFerry 是一个自托管的内网穿透和流量转发系统。Controller 负责身份、权限、配置、调度和审计；每台数据面主机只运行一个通用 Node。Node 注册完成后，再由 Dashboard 将它配置为 Gateway 或 Agent。

```text
Dashboard / CLI -- HTTPS --> Controller -- mTLS gRPC --> Node
                                      |
                                      +-- SQLite（默认）或 PostgreSQL

Gateway <========== AFDP/2 over QUIC ==========> Agent
```

本文介绍 Linux/Windows Controller 和 Linux/Windows Node 部署。Controller 的 HTTPS 和 gRPC 都可以绑定 `0.0.0.0`，但 `--grpc-advertise` 必须填写 Node 实际可访问的公网 IP、内网 IP 或域名，不能填写 `0.0.0.0`。

## 端口和前置条件

| 端口 | 用途 |
| --- | --- |
| TCP 8443 | HTTPS API 和 Dashboard |
| TCP 9443 | Node 连接 Controller 的 mTLS gRPC |
| UDP 4433 | Gateway 默认 AFDP/2 数据端口 |
| TCP 9090 | Prometheus 指标，默认只监听本机 |

请在云安全组和主机防火墙放行实际需要的端口。源码构建使用仓库 `.toolchain.json` 中的 Go、Node.js 和 npm 版本。

## 方式一：下载源码、构建并启动

### 1. 构建 Controller 和 Node

```bash
git clone https://github.com/eternallyzzz/asterferry.git
cd asterferry

npm --prefix web/dashboard ci --audit=false \
  --registry=https://registry.npmjs.org \
  --replace-registry-host=always
npm --prefix web/dashboard run build

mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -tags=dashboard_assets -trimpath -ldflags="-s -w" \
  -o dist/asterferry-linux-amd64 ./cmd/asterferry
```

`dashboard_assets` 会把 Vue Dashboard 编进 Controller 二进制；不带这个 tag 构建的 Controller 只会显示 “Dashboard assets are not embedded”。构建 ARM64 时将 `GOARCH=arm64` 和输出文件名改为 `arm64`。

Windows 构建使用管理员 PowerShell 之外的普通开发终端即可：

```powershell
npm --prefix web/dashboard ci --audit=false
npm --prefix web/dashboard run build
$env:CGO_ENABLED = "0"
go build -tags=dashboard_assets -trimpath -ldflags="-s -w" -o dist/asterferry.exe ./cmd/asterferry
```

### 2. 初始化并启动 Controller

下面的示例让 Controller 对外监听 `0.0.0.0`，并把公网地址设为 `47.98.144.86`。请按实际部署修改 `--grpc-advertise`。

```bash
sudo useradd --system --home-dir /var/lib/asterferry \
  --shell /usr/sbin/nologin asterferry 2>/dev/null || true
sudo install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
sudo install -d -o asterferry -g asterferry -m 0750 /var/lib/asterferry/bin
sudo install -o asterferry -g asterferry -m 0755 \
  dist/asterferry-linux-amd64 /var/lib/asterferry/bin/asterferry

sudo -u asterferry /var/lib/asterferry/bin/asterferry controller init \
  --dir /var/lib/asterferry \
  --http-listen 0.0.0.0:8443 \
  --grpc-listen 0.0.0.0:9443 \
  --grpc-advertise 47.98.144.86:9443

sudo install -m 0644 deploy/asterferry-controller.service \
  /etc/systemd/system/asterferry-controller.service
sudo systemctl daemon-reload
sudo systemctl enable --now asterferry-controller.service
```

初始化输出中的 Admin 密码只显示一次，请立即保存。Dashboard 地址为 `https://47.98.144.86:8443/dashboard/`。

### 3. 本地构建 Node 的注册和启动

本地开发或尚未发布的源码不能从 GitHub 下载 Node Release，因此在这种情况下直接把同一个 Node 二进制复制到 B、C 两台机器，并在 Dashboard 中生成通用注册 Token。只复制 `ca/ca.crt`，不要复制 `ca/ca.key`。

在 B、C 上执行 Dashboard 生成的命令。命令中已经包含 Controller 自动生成的唯一 Node ID 和一次性 Token，不要手工改写 Node ID：

```bash
sudo useradd --system --home-dir /var/lib/asterferry \
  --shell /usr/sbin/nologin asterferry 2>/dev/null || true
sudo install -d -o asterferry -g asterferry -m 0700 /var/lib/asterferry
sudo install -d -o asterferry -g asterferry -m 0750 /var/lib/asterferry/bin
sudo install -o asterferry -g asterferry -m 0755 \
  dist/asterferry-linux-amd64 /var/lib/asterferry/bin/asterferry
sudo install -o asterferry -g asterferry -m 0644 controller-ca.crt \
  /var/lib/asterferry/controller-ca.crt

sudo -u asterferry /var/lib/asterferry/bin/asterferry node enroll \
  --controller 47.98.144.86:9443 \
  --token '<token>' \
  --node-id '<controller-generated-node-id>' \
  --ca /var/lib/asterferry/controller-ca.crt \
  --output /var/lib/asterferry/node-bootstrap.json \
  --cache /var/lib/asterferry/snapshot.cache

sudo install -m 0644 deploy/asterferry-node.service \
  /etc/systemd/system/asterferry-node.service
sudo systemctl daemon-reload
sudo systemctl enable --now asterferry-node.service
```

注册成功后，Node 默认没有角色。先在 Dashboard 为 C 选择 Gateway 并配置公网端点，再为 B 选择 Agent 并绑定 C；角色配置不会写入安装命令。底层 CLI 仍保留显式 `--node-id`，仅用于源码开发和兼容已有自动化脚本。

如果源码来自一个已经发布的版本，也可以把 `scripts/install-node.sh`、`scripts/install-node.ps1` 和该版本的 `node-release.json` 放入 `/var/lib/asterferry/node-installers/`，之后使用 Dashboard 生成的一键安装命令。

## 方式二：下载脚本安装最新 Release

### 1. 安装 Controller

源码中的安装脚本从 GitHub 获取最新稳定语义化 Release，下载并校验 Controller，同时把 Node 安装脚本和 Node Release 元数据放入 Controller 数据目录；不会把 Node 二进制提前下载到 Controller。发布资产中的同名脚本会固定到它所属的具体 Release，因此不需要再输入 GitHub 下载地址或版本。

```bash
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.3 \
  https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.sh \
  | sudo bash -s -- --grpc-advertise 47.98.144.86:9443
```

脚本默认监听 `0.0.0.0:8443` 和 `0.0.0.0:9443`。原生 Linux 或启用了 systemd 的 WSL2 会创建并启动 `asterferry-controller.service`；未启用 systemd 的 WSL2 会自动使用 WSL 后台进程和启动钩子。首次生成的 Admin 密码会在输出中显示一次。

Windows 11 启用 `sudo` 后，在普通 PowerShell 中下载并执行源码入口脚本：

```powershell
$installer = Join-Path ([IO.Path]::GetTempPath()) "asterferry-install-controller.ps1"
try {
  curl.exe --fail --silent --show-error --location --proto '=https' --tlsv1.3 `
    "https://raw.githubusercontent.com/eternallyzzz/asterferry/main/scripts/install-controller.ps1" `
    --output $installer
  if ($LASTEXITCODE -ne 0) { throw "failed to download the Controller installer" }
  sudo pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass -File $installer
  if ($LASTEXITCODE -ne 0) { throw "Controller installer failed" }
} finally {
  Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue
}
```

脚本只会询问 Node 可访问的 Controller gRPC 广播地址；HTTPS 和 gRPC 默认分别监听 `0.0.0.0:8443`、`0.0.0.0:9443`，其他安装目录、服务名和用户名直接使用默认值。它会下载 Windows Controller、Node 安装脚本和发布元数据，创建并启动 `AsterFerry-Controller` 服务。首次生成的 Admin 密码会在输出中显示一次。

需要固定到某个已发布版本时，直接下载该版本的发布资产即可；下面的脚本已经内置 `v1.0.0-rc.2` 的下载地址和版本，不需要填写 `-ReleaseBaseUrl` 或 `-Version`：

```powershell
$installer = Join-Path ([IO.Path]::GetTempPath()) "asterferry-install-controller.ps1"
try {
  curl.exe --fail --silent --show-error --location --proto '=https' --tlsv1.3 `
    "https://github.com/eternallyzzz/asterferry/releases/download/v1.0.0-rc.2/install-controller.ps1" `
    --output $installer
  if ($LASTEXITCODE -ne 0) { throw "failed to download the Controller installer" }
  sudo pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass -File $installer
  if ($LASTEXITCODE -ne 0) { throw "Controller installer failed" }
} finally {
  Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue
}
```

未启用 Windows `sudo` 时，请在“管理员：PowerShell”中执行相同命令，并将 `sudo pwsh` 改为 `pwsh`。

### 2. 安装通用 Node

登录 Dashboard，在 **节点** 页面创建安装任务，选择 Linux/Windows、架构和“安装脚本来源”：

- `Controller`（推荐）：安装命令从当前 Controller 下载 Node 安装脚本；
- `GitHub Release`：安装命令从对应 GitHub Release 下载 Node 安装脚本。

两种来源随后都会使用 Controller 返回的版本元数据，从 GitHub Release 下载匹配架构的 Node 二进制并校验 `SHA256SUMS`。B、C 都执行各自生成的一行命令，安装时不选择角色。Windows 命令在管理员 PowerShell 中直接粘贴执行，Linux 命令在 root shell 中直接粘贴执行；不需要手动下载 CA、二进制或服务文件。

Linux 安装器在原生 Linux 或启用了 systemd 的 WSL2 中创建并启动 systemd 服务。未启用 systemd 的 WSL2 也可以直接安装：安装器会改用受 `asterferry` 用户管理的后台进程，记录 PID 和日志，并在 `/etc/wsl.conf` 写入 WSL 启动钩子；不需要先打开 systemd。配置会保留原有的 `[network]` 等段落，并在数据目录的 `recovery/` 下备份被替换的 `wsl.conf`。首次安装后从 Windows 执行一次 `wsl --shutdown`，以后重新启动该发行版时节点会自动拉起。

未启用 systemd 的 WSL2 使用以下命令查看状态和日志：

```bash
sudo /usr/local/sbin/asterferry-controller-wsl-start status /var/lib/asterferry
sudo /usr/local/sbin/asterferry-node-wsl-start status /var/lib/asterferry
tail -f /var/lib/asterferry/controller.log
tail -f /var/lib/asterferry/node.log
```

如果希望 WSL 使用完整的 systemd，也可以在 `/etc/wsl.conf` 增加 `[boot] systemd=true`，然后从 Windows 执行 `wsl --shutdown`；重新运行安装命令后会自动切换回标准 systemd 服务。

安装完成后回到 Dashboard：C 选择 Gateway 并填写公网 AFDP/2 端点，B 选择 Agent 并从已注册 Gateway 下拉框选择 C。

### 3. 验证服务

```bash
sudo systemctl status asterferry-controller.service
sudo systemctl status asterferry-node.service
sudo journalctl -u asterferry-controller.service -f
sudo journalctl -u asterferry-node.service -f
curl -k https://47.98.144.86:8443/healthz
```

WSL 未启用 systemd 时，使用安装器输出的 WSL 管理命令和 `controller.log`、`node.log` 查看状态，不使用 `systemctl`。

## 自动检测和升级

Controller 默认每 6 小时从 GitHub 查询最新的正式版 Release；`-rc`、草稿和其他预发布版本不会触发自动升级。Controller 会先在 Dashboard 的管理页面显示可用版本，只有管理员明确确认后才会下载带 `SHA256SUMS` 校验的归档，并在 `/readyz` 恢复失败时自动回滚。

原生 Windows 服务、Linux systemd 和无 systemd 的 WSL Node 支持由 Controller 逐台下发 Node 自升级。旧安装如果没有 `node-upgrade-v1` 能力，必须先用一次新的安装脚本升级；容器和 Helm 部署只报告需要滚动更新镜像，不会替换容器内的二进制。升级会保留数据库、CA、TLS 身份、master key、bootstrap、缓存和配置。

除 Dashboard 外，也可以使用 CLI（需要 HTTPS API Token）：

```bash
asterferry controller update status --url https://controller.example:8443 --token "$ASTERFERRY_API_TOKEN"
asterferry controller update check --url https://controller.example:8443 --token "$ASTERFERRY_API_TOKEN"
asterferry controller update apply --url https://controller.example:8443 --token "$ASTERFERRY_API_TOKEN"
asterferry controller update node --node-id '<node-id>' \
  --url https://controller.example:8443 --token "$ASTERFERRY_API_TOKEN"
```

升级 Controller 前仍应先完成一次可恢复的备份；CLI 的 `--insecure-tls` 只适合本机临时探测。

## 节点退役、替换和永久删除

“退役”会撤销旧证书并保留 Service、规格和审计数据。原机器上的旧 Node 不会凭旧证书自动重新注册；要复用原机器，在节点详情生成替换注册命令即可，无需卸载二进制。替换命令会清理旧身份和缓存、申请新证书并重新启动服务。

“永久删除”只允许删除已经退役且没有 Service、Assignment 或 Agent-Gateway 依赖的节点；它会删除节点身份、证书、规格、快照和运行状态，不会级联删除业务 Service。

## 当前版本边界

本安装协议是新协议，不兼容旧版 `controller.json` 中的 `release_base_url`、`release_version` 字段，也不兼容旧 Node 安装命令中的 `--version`、`--arch`、`--repo` 和 `--release-base-url` 参数。请使用同一版本的新 Controller、安装脚本和 Node 安装命令重新部署。

## 备份与参考

Controller 备份应包含数据库、CA、TLS 身份和 master key：

```bash
sudo -u asterferry /var/lib/asterferry/bin/asterferry controller backup \
  --config /var/lib/asterferry/controller.json \
  --output /var/backups/asterferry
```

- [架构与契约](docs/architecture.md)
- [中文运维指南](docs/operations.zh-CN.md)
- [兼容性说明](docs/compatibility.md)
- [支持矩阵](docs/support-matrix.md)
- [发布流程](docs/release-runbook.md)
- [安全策略](SECURITY.md)
