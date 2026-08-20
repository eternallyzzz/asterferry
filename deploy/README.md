# Linux production deployment

1. Create a dedicated service account and directories:

   ```sh
   useradd --system --home-dir /var/lib/asterferry --shell /usr/sbin/nologin asterferry
   install -d -o asterferry -g asterferry -m 0750 /etc/asterferry /var/lib/asterferry
   install -o root -g asterferry -m 0750 asterferry /usr/local/bin/asterferry
   ```

2. Place configuration, CA files, certificates, private keys, and tokens in
   `/etc/asterferry`. Private keys and tokens should be mode `0600` and owned
   by `asterferry`. The Gateway and Agent `transport.alpn` values must match,
   but do not use the repository example value.

3. Validate before replacing a running configuration:

   ```sh
   /usr/local/bin/asterferry validate -c /etc/asterferry/gateway.yaml
   /usr/local/bin/asterferry validate -c /etc/asterferry/agent.yaml
   ```

4. Install and start the appropriate service:

   ```sh
   install -m 0644 deploy/asterferry-gateway.service /etc/systemd/system/
   systemctl daemon-reload
   systemctl enable --now asterferry-gateway
   systemctl status asterferry-gateway
   ```

   Use `asterferry-agent.service` on internal nodes. The Gateway firewall
   should allow only the QUIC UDP port and explicitly configured reverse
   TCP/UDP ports. Keep the 9090/9091 management endpoints on loopback.

5. Rotate certificates or configuration using a validate-then-restart process.
   Agents reconnect automatically with exponential backoff. During a Gateway
   restart, new business connections are rejected and existing connections are
   closed cleanly.
