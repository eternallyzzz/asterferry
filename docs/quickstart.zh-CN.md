# AsterFerry 从零开始部署（中文）

本文是一份可以照着执行的端到端上手指南。示例拓扑包含三台服务器：

```text
                         C：Controller
                 REST/Dashboard 8443/tcp
                       mTLS gRPC 9443/tcp
                              ▲
                    A、B 均主动连接 C:9443
                              │
                              │ 控制面：配置、证书、快照、调度
                              │
 A：内网 Agent  ──────── UDP 4433 ────────>  B：公网 Gateway
 内部服务 127.0.0.1:18080                         对外服务端口 28080/tcp
                                                  对外服务端口 28081/udp

                   业务数据不经过 C
 客户端 ───────────────> B:28080 ── AFDP/2 ──> A:18080
```

## 1. 节点行为和端口

| 节点 | 作用 | 必须连通的地址 |
| --- | --- | --- |
| C · Controller | 身份、RBAC、配置、快照、调度、Dashboard 和审计 | 管理员访问 `8443/tcp`；A、B 访问 `9443/tcp` |
| B · Gateway | 监听公网 AFDP/2 和服务端口，承载公网连接 | `4433/udp` 接收 A 的 Agent 连接；`28080/tcp`、`28081/udp` 等服务端口接收客户端连接 |
| A · Agent | 连接 C 和 B，并访问 A 上的内网服务 | 出站访问 `C:9443`、`B:4433/udp`；访问本机 `127.0.0.1:18080` |

几个端口的含义不要混淆：

- `C:8443` 是 Controller 的 HTTPS REST API 和 Dashboard，不是业务转发端口。
- `C:9443` 是 Agent/Gateway 到 Controller 的 mTLS gRPC 控制通道。
- `B:4433/udp` 是 Gateway 的 AFDP/2 端点，Agent 会主动拨号连接它。
- `B:28080/tcp` 是业务对外端口；`A:18080` 是 Agent 所在机器上的实际内网目标。
- Controller 不承载业务数据，业务数据路径是客户端 → Gateway → AFDP/2 → Agent → 内部服务。

下面使用示例地址，请替换成真实地址：

| 占位符 | 示例值 | 说明 |
| --- | --- | --- |
| `C_HOST` | `controller.example.com` | C 的稳定 DNS 名称或本机可绑定的固定 IP |
| `B_HOST` | `gateway.example.com` | B 的公网 DNS 名称或公网 IP |
| `A_SERVICE` | `127.0.0.1:18080` | A 上实际运行的内网服务 |

## 2. 准备二进制和网络

可以使用发布包，也可以从源码构建。源码构建需要 Go `1.26.7`；单独构建 Dashboard 需要 Node.js 24 和 npm 11+。

在源码目录构建：

```powershell
# Windows
go build -o asterferry.exe ./cmd/asterferry
```

```sh
# Linux / WSL
go build -o asterferry ./cmd/asterferry
```

把二进制放到 C、B、A 三台机器，并确保三台机器运行同一版本。当前版本只支持 AFDP/2；AFDP/1 与 AFDP/2 不兼容。

下文的 `asterferry` 表示这个二进制：Windows PowerShell 请替换为
`.\asterferry.exe`，Linux/WSL 请使用 `./asterferry`；PowerShell 和 POSIX
shell 代码块不要混用。

生产环境建议先配置 DNS 或 `/etc/hosts`，使 C、B 的名称在相关机器上都能解析。例如：

```text
192.0.2.30   controller.example.com
198.51.100.20 gateway.example.com
```

防火墙至少需要允许：

- C：从管理端到 `8443/tcp`；从 A、B 到 `9443/tcp`。
- B：从 A 到 `4433/udp`；从业务客户端到已配置的 TCP/UDP 服务端口。
- A：Agent 进程能够访问 `A_SERVICE`。如果目标绑定在 `127.0.0.1`，Agent 必须运行在同一台机器上。

## 3. 在 C 上初始化并启动 Controller

### 3.1 创建管理员密码

可以让 CLI 生成随机初始密码，也可以通过受保护的文件指定密码。使用 `--password-file` 时，文件只在初始化时读取：

```powershell
# Windows PowerShell：不会在屏幕回显输入内容；使用无 BOM UTF-8，兼容 Windows PowerShell 5.1
$AdminPassword = Read-Host "Controller Admin password"
$Utf8NoBom = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false
[System.IO.File]::WriteAllText((Join-Path (Get-Location) 'admin-password.txt'), $AdminPassword, $Utf8NoBom)
Remove-Variable AdminPassword, Utf8NoBom
```

```sh
# Linux / WSL
umask 077
read -r -s ADMIN_PASSWORD
printf '%s\n' "$ADMIN_PASSWORD" > ./admin-password.txt
unset ADMIN_PASSWORD
```

不要把密码直接写在命令行中，以免进入 shell history。初始化完成后应妥善保存密码文件，并按组织策略删除临时文件。

### 3.2 初始化

`--http-listen`、`--grpc-listen` 和 `--grpc-advertise` 应使用稳定的 C 主机名或 IP。初始化时生成的 Controller 证书会把这些地址写入证书 SAN；不要在这个远程部署示例中用 `0.0.0.0` 代替稳定名称。

```powershell
# Windows；把 C_HOST 替换成 C 的真实 DNS 或固定 IP
asterferry.exe controller init `
  --dir .\controller `
  --username admin `
  --password-file .\admin-password.txt `
  --http-listen controller.example.com:8443 `
  --grpc-listen controller.example.com:9443 `
  --grpc-advertise controller.example.com:9443 `
  --release-version 1.0.0
```

```sh
# Linux / WSL
./asterferry controller init \
  --dir ./controller \
  --username admin \
  --password-file ./admin-password.txt \
  --http-listen controller.example.com:8443 \
  --grpc-listen controller.example.com:9443 \
  --grpc-advertise controller.example.com:9443 \
  --release-version 1.0.0
```

`--grpc-advertise` 是 A、B 实际连接的 C 地址；`--release-version` 必须替换为已经发布到 `release_base_url` 的版本。初始化会把广播地址写入 Controller 证书 SAN。自建发布镜像可通过 `--release-base-url` 指定，但必须是 HTTPS。

如果已有 Controller 初始化时没有填写广播地址，先停止 Controller，再执行下面的命令修复；它不会替换数据库、CA、master key 或管理员账号：

```powershell
asterferry.exe controller configure `
  --config .\controller\controller.json `
  --grpc-advertise controller.example.com:9443
```

如果旧配置也没有已发布的 `release_version`，再加上
`--release-version 1.0.0`；使用私有 HTTPS 发布镜像时再加
`--release-base-url`。

该命令会重新签发包含新地址 SAN 的 Controller 证书。重启 Controller 后再生成节点安装命令。Windows Controller 与本机 WSL 联调时，WSL 通常可访问 `172.28.80.1:9443`；远程节点必须使用 C 的稳定局域网地址或 DNS，不能使用这个 WSL 虚拟地址。

不提供 `--password-file` 或 `--password` 时，CLI 会生成随机密码并在初始化输出中显示一次。密码不会保存到 `controller.json`。

### 3.3 启动并检查

```powershell
asterferry.exe controller run --config .\controller\controller.json
```

```sh
./asterferry controller run --config ./controller/controller.json
```

另开一个终端检查健康状态：

```sh
asterferry healthcheck \
  --url https://controller.example.com:8443/healthz \
  --insecure-tls
```

`--insecure-tls` 只用于本地健康探测。Dashboard 浏览器访问：

```text
https://controller.example.com:8443/dashboard/
```

初始化会生成自签名 CA。生产环境应把 `controller/ca/ca.crt` 安装到管理端的信任库；临时开发环境可以手动接受浏览器证书警告。不要把 `ca.key`、`master.key` 或 Controller TLS 私钥复制到 A、B。

## 4. 在 Dashboard 生成安装命令并注册 A、B

登录 `https://controller.example.com:8443/dashboard/`，打开“节点”页。
所有业务配置仍由 Controller 管理，A、B 不读取业务 YAML。

### 4.1 创建第一个 Node 安装任务

点击“生成安装命令”，填写 Node ID、名称、标签，再选择目标机器的平台和架构。这里不选择 Gateway/Agent，也不填写 AFDP 地址；创建的是“待安装任务”，不会立即创建正式节点。

提交后 Dashboard 会返回一条只显示一次的安装命令。把 B 的命令复制到 B、把 A 的命令复制到 A：Linux 在 root shell 执行，Windows 在“以管理员身份运行”的 PowerShell 执行。目标机器执行命令并首次连接 Controller 完成 Enroll 后，Node 才会出现在正式节点列表。

### 4.2 在 Node 详情配置行为

节点上线后，打开对应 Node 的“详情”→“规格”，选择行为并保存：

- B 选择 `Gateway`，在规格 JSON 中配置 `public_endpoints`、监听器和 TCP/UDP 端口池；`public_endpoints` 是 A 连接 B 的数据面地址，不是 Controller 地址。
- A 选择 `Agent`，配置 `gateway_selector`、代理入口和路由。

行为保存后，Controller 才会向 Node 下发对应数据面快照。删除规格会让 Node 回到未配置状态；切换已有行为前需先删除当前规格。

安装命令会下载与 Controller 配置版本完全一致的官方发布包，校验 SHA-256，写入 CA 和 bootstrap/cache 私有目录，调用一次性节点 Token 完成证书 enrollment，并创建、启用和启动系统服务：

- Linux：`asterferry-Node.service`。
- Windows：`AsterFerry-Node` Windows 服务。

Token 默认 15 分钟有效，只能用于创建它对应的 Node ID。命令不要发到公开聊天、工单或日志中；过期或丢失时，在“待安装任务”中重新生成即可。B、A 不需要手工复制 CA、编辑 bootstrap JSON 或执行第二条启动命令。

### 4.3 旧版/高级手工 enrollment

仍可使用 `enroll-token create`、`node enroll`、`node run` 和 systemd/Windows 服务模板完成手工部署，适合离线镜像、私有发布源或自定义服务账户。所有部署都使用统一的 `node` 命令，行为只由 Controller 中的 Node 规格决定。

## 5. 创建第一个 TCP 服务

先确保 A 上的真实服务已经监听 `127.0.0.1:18080`。例如仅用于测试：

```sh
python3 -m http.server 18080 --bind 127.0.0.1
```

在 Dashboard 进入“服务” →“创建服务”：

| 字段 | 值 |
| --- | --- |
| Service ID | `internal-web` |
| Agent | `agent-internal` |
| 协议 | `TCP` |
| 公网端口 | `28080` |
| 本地目标 | `127.0.0.1:18080` |
| 公网绑定 | `0.0.0.0` |
| Gateway selector JSON | `{"match_labels":{"site":"public"}}` |
| 启用服务 | 勾选 |

保存后，Controller 会自动选择匹配且健康的 Gateway 并生成 Assignment；通常不需要再点“重新调度”。如果 Gateway 尚未上线或端口池暂时没有可用端口，服务会保持待调度状态，资源恢复后 Controller 会自动重试。

`public_port` 填 `0` 表示由 Gateway 的端口池自动分配；本例填 `28080`，因此客户端访问 `B:28080`。

UDP 服务的配置方式相同，只需把协议改为 UDP、端口改为 `28081`，并确保 A 上的 UDP 目标可访问。

## 6. 验证链路

在 Dashboard 检查：

1. “节点” → 对应 Node →“观测”：节点应为健康，已应用 Generation 应大于 0；“信息”中的行为规格应显示 Gateway 或 Agent。
2. “调度”：Assignment 应为 `applied`，端点应显示 `gateway.example.com:4433`，绑定应显示 `28080/tcp`。
3. “活动”：可以看到节点、服务、调度和运行时事件。

从不在 A 上的客户端访问：

```sh
curl http://gateway.example.com:28080/
```

如果能看到 A 上 Python HTTP server 的目录页，完整路径就是：

```text
客户端 → B:28080 → B Gateway → AFDP/2 UDP 4433 → A Agent → A:18080
```

常用检查：

```powershell
# Windows 客户端检查 Controller 的 TCP 端口
Test-NetConnection controller.example.com -Port 8443
Test-NetConnection controller.example.com -Port 9443

# 检查 Gateway 的 TCP 业务端口
Test-NetConnection gateway.example.com -Port 28080
```

UDP 端口不能用 TCP 检查工具判断是否可用，应结合 Dashboard 的 Assignment/观测状态、节点日志和防火墙抓包验证。

## 7. WSL 与 Windows 宿主机通信

### 7.1 最简单的本机开发方式

如果 Controller、Gateway、Agent 都在同一个 WSL 实例中运行：

- Controller 可以使用 `localhost:8443` 和 `localhost:9443` 初始化。
- Gateway 的 `public_endpoints` 可以使用 `127.0.0.1:4433`。
- Windows 浏览器通常可以访问 WSL 中的 `https://localhost:8443/dashboard/`。
- 业务客户端在 Windows 上访问 `localhost:28080` 时，前提是该 TCP 端口被 WSL localhost forwarding 转发。

如果 A、B、C 分别在不同机器或不同网络命名空间，不能使用 `127.0.0.1` 作为跨主机地址，必须使用实际可达的 DNS/IP。

### 7.2 WSL 访问 Windows 宿主机服务

WSL2 默认 NAT 模式下，Linux 进程访问 Windows 上的服务通常使用宿主机 IP：

```sh
WINDOWS_HOST=$(ip route show | awk '/default/ {print $3; exit}')
curl "http://${WINDOWS_HOST}:18080/"
```

因此，如果 Agent 在 WSL、真实内网服务在 Windows，Dashboard 的 `本地目标` 应填类似 `${WINDOWS_HOST}:18080` 的实际地址，而不是无条件填 `127.0.0.1:18080`。Windows 服务还必须监听一个 WSL 可达的地址，而不是只监听 Windows 的回环地址。

Windows 11 22H2 及更高版本也可以使用 WSL mirrored networking，让 Windows 和 WSL 共享网络接口；此时可按当前 WSL 配置使用 IPv4 `localhost`，但仍需配置 Windows 防火墙。

在 `%USERPROFILE%\.wslconfig` 中启用，然后重启 WSL：

```ini
[wsl2]
networkingMode=mirrored
```

```powershell
wsl --shutdown
```

### 7.3 从局域网访问 WSL 中的 TCP 服务

WSL2 NAT 地址可能在重启后变化。需要从 Windows 宿主机对外发布 TCP 端口时，可以用管理员 PowerShell 创建端口转发：

```powershell
$WslIp = (wsl.exe hostname -I).Trim().Split()[0]
foreach ($Port in @(8443, 9443, 28080)) {
  netsh interface portproxy add v4tov4 `
    listenaddress=0.0.0.0 `
    listenport=$Port `
    connectaddress=$WslIp `
    connectport=$Port
}

netsh interface portproxy show all
```

只开放必要端口，并用 Windows 防火墙限制来源网段。WSL IP 变化后需要更新规则；删除规则示例：

```powershell
netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=8443
netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=9443
netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=28080
```

`netsh interface portproxy` 是 TCP 端口转发，不能转发 Gateway 的 `4433/udp`，也不能转发 UDP 业务端口。要让外部客户端或 A 访问 WSL 中的 Gateway UDP 端点，应选择以下方案之一：

1. 在 Windows 11 22H2+ 使用 mirrored networking，并配置 Windows 防火墙放行 UDP。
2. 使用具备 UDP 转发能力的专用 NAT/转发器。
3. 把 Gateway 放到真实公网主机 B 上，把 WSL 只作为开发环境或 Agent 环境。

WSL 网络模式、localhost forwarding 和 `netsh portproxy` 的背景说明见 [Microsoft WSL networking 文档](https://learn.microsoft.com/en-us/windows/wsl/networking)。

## 8. 常见问题

### Dashboard 打不开

检查 C 的 `8443/tcp`、Controller 是否运行，以及浏览器是否信任 `controller/ca/ca.crt`。Dashboard 地址是 `/dashboard/`，不是 `/`。

### enrollment 报 TLS 或证书名称错误

确认：

- enrollment 使用了 C 的 CA 证书，而不是 B/A 的文件。
- `--server-name` 与 Controller 证书 SAN 一致。
- `controller init` 时 `--http-listen`、`--grpc-listen` 和 `--grpc-advertise` 使用了稳定的 C 主机名或 IP；广播地址也会写入证书 SAN。
- A、B 访问的是 `C_HOST:9443`，且防火墙允许 TCP 9443。

仅用于临时本地实验时可以显式使用 `--insecure`，生产环境不要这样做。

### Assignment 一直是 pending

检查 Gateway 是否已注册、证书是否 active、节点是否 enabled、Gateway 的标签是否匹配 Agent/Service selector，以及 Gateway 的 TCP/UDP 端口池是否有空闲端口。也可以在“调度”页点击“重新调度”。

### B 端口没有监听

Gateway 只有在收到并应用有效快照、Assignment 状态为 applied 后才会创建业务监听器。先看“观测”和“调度”，再检查 B 的 UDP `4433` 和业务端口防火墙。

### 访问 B:28080 失败

确认 A 上的 `127.0.0.1:18080` 是从 Agent 进程所在环境可访问的；`local_target` 是 Agent 的视角，不是 Controller 或客户端的视角。随后检查 Agent 到 B 的 UDP 4433 出站/入站路径。

### 修改配置后节点没有变化

节点通过 mTLS gRPC 从 Controller 拉取带 generation 和 checksum 的快照。检查 Controller、节点日志和“观测”中的已应用 Generation。不要直接编辑数据库或 bootstrap JSON。

## 9. 备份、升级和停机

升级前先备份完整 Controller 目录：

```sh
asterferry controller backup \
  --config ./controller/controller.json \
  --output ./backups
```

当前 pre-1.0 版本使用 SQLite schema v9。v8 数据库启动时会执行一次附加式
运行时观测迁移；更早或未知代际仍会被 `OpenStore` 拒绝。升级前备份完整
Controller 目录并保留回滚副本，在维护窗口执行升级。运行时观测和高级
操作开关见[运维指南](operations.zh-CN.md)。

Controller 暂时不可用时，节点会继续使用加密的 last-known-good 快照；但新的配置、调度和证书操作需要 Controller 恢复后才能完成。不要删除 `controller/`，其中包含数据库、CA、TLS 身份和 master key。

更多 systemd、Docker Compose 和 Kubernetes 部署细节见 [`deploy/README.md`](../deploy/README.md)。

## 10. 完成清单

- [ ] C 的 `8443/tcp` 和 `9443/tcp` 已监听并可达。
- [ ] Dashboard 可以登录，两个统一 Node 已完成安装并注册。
- [ ] Gateway 行为的 `public_endpoints` 使用 B 的可达 `4433/udp` 地址。
- [ ] Agent selector 与 Gateway 标签匹配。
- [ ] A、B 已完成 Node 一次性 token enrollment，并分别保存了 Agent/Gateway 行为规格。
- [ ] 节点证书为 active，观测健康，Assignment 为 applied。
- [ ] Service 的 `local_target` 从 A 的 Agent 进程视角可访问。
- [ ] 客户端可以访问 B 的业务端口。
- [ ] bootstrap、CA、Controller 数据目录已按密钥策略保护并备份。
