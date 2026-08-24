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
   /usr/local/bin/asterferry validate --config /etc/asterferry/gateway.yaml
   /usr/local/bin/asterferry validate --config /etc/asterferry/agent.yaml
   /usr/local/bin/asterferry doctor --config /etc/asterferry/gateway.yaml --skip-ports
   /usr/local/bin/asterferry doctor --config /etc/asterferry/agent.yaml --skip-ports
   ```

   `status --config ...` queries the loopback management endpoint of a running
   role and uses the configured management token automatically.

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
   Agents reconnect automatically with exponential backoff. On the first
   `SIGTERM`/`SIGINT`, the service marks itself unready, rejects new traffic,
   and drains admitted connections for `shutdown.grace_period_seconds` (30
   seconds by default). The bundled unit uses `TimeoutStopSec=35s`; increase it
   if the configuration uses a longer drain period. A second signal forces an
   immediate close.
