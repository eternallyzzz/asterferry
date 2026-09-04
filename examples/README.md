# Controller API examples

For a complete Controller + public Gateway + private Agent deployment, see
the [English end-to-end quick start](../docs/quickstart.en.md) or the
[中文端到端快速开始](../docs/quickstart.zh-CN.md).

Business configuration is owned by the Controller and is not represented by
repository YAML files. Use the Dashboard or the `/api/v1` API to create Nodes,
select Gateway/Agent behavior specs, services and assignments.

## Start a local Controller

```powershell
asterferry controller init --dir ./controller --username admin --password-file ./admin-password.txt
asterferry controller run --config ./controller/controller.json
```

Log in to `https://localhost:8443/dashboard/` (or use the REST API), create one
generic Node installation for each host, and run the generated command on the
target host. Enrollment produces the bootstrap identity consumed by the node
process:

```powershell
asterferry enroll-token create --config ./controller/controller.json --node-id gw-local
asterferry node enroll --controller localhost:9443 --token <token> --node-id gw-local --ca ./controller/ca/ca.crt --output ./node-bootstrap.json
asterferry node run --bootstrap ./node-bootstrap.json
```

Repeat the generic enrollment command for the internal node. After both nodes
connect, save Gateway/Agent behavior under each Node's `spec`, then create services from the Dashboard wizard or with JSON requests
to `/api/v1/services`; the scheduler assigns each enabled service to a healthy
Gateway and allocates a port from that Gateway's configured pool.

Never commit bootstrap files, private keys, passwords, or API tokens. The
Controller database defaults to the v12 SQLite store. Larger deployments can
initialize with `--database-driver postgres --database-url 'postgres://...'`.
Backend changes are clean-break development operations: initialize a new
Controller and recreate resources; there is no SQLite-to-PostgreSQL migration
command.
