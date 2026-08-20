# Linux 生产部署

1. 创建专用用户和目录：

   ```sh
   useradd --system --home-dir /var/lib/asterferry --shell /usr/sbin/nologin asterferry
   install -d -o asterferry -g asterferry -m 0750 /etc/asterferry /var/lib/asterferry
   install -o root -g asterferry -m 0750 asterferry /usr/local/bin/asterferry
   ```

2. 将配置、CA、证书、私钥和 token 放入 `/etc/asterferry`。私钥和 token 应为
   `0600`，属主为 `asterferry`。Gateway 和 Agent 的 `transport.alpn` 必须一致，
   但不要使用仓库示例值。

3. 在替换配置前校验：

   ```sh
   /usr/local/bin/asterferry validate -c /etc/asterferry/gateway.yaml
   /usr/local/bin/asterferry validate -c /etc/asterferry/agent.yaml
   ```

4. 安装对应 service 文件并启动：

   ```sh
   install -m 0644 deploy/asterferry-gateway.service /etc/systemd/system/
   systemctl daemon-reload
   systemctl enable --now asterferry-gateway
   systemctl status asterferry-gateway
   ```

   内网节点使用 `asterferry-agent.service`。Gateway 防火墙只放行 QUIC UDP 端口和
   明确配置的反向 TCP/UDP 端口；9090/9091 管理端点保持 loopback。

5. 证书或配置轮换采用“校验后重启”流程。Agent 使用指数退避自动重连，Gateway 重启
   期间不会接受新业务连接，已有连接会安全关闭。
