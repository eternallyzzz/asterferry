# Controller API examples

Business configuration is owned by the Controller and is not represented by
repository YAML files. Use the Dashboard or the `/api/v1` API to create nodes,
Gateway/Agent specs, services and assignments.

## Start a local Controller

```powershell
asterferry controller init --dir ./controller --username admin --password-file ./admin-password.txt
asterferry controller run --config ./controller/controller.json
```

Log in to `https://localhost:8443/dashboard/` (or use the REST API), register
one Gateway and one Agent, and create a role-bound enrollment token for each
node. Enrollment produces the bootstrap identity consumed by the node process:

```powershell
asterferry enroll-token create --config ./controller/controller.json --role gateway
asterferry gateway enroll --controller localhost:9443 --token <token> --node-id gw-local --ca ./controller/ca/ca.crt --output ./gw-bootstrap.json
asterferry gateway run --bootstrap ./gw-bootstrap.json
```

Repeat the enrollment commands with `agent` for the internal node. After both
nodes connect, create services from the Dashboard wizard or with JSON requests
to `/api/v1/services`; the scheduler assigns each enabled service to a healthy
Gateway and allocates a port from that Gateway's configured pool.

Never commit bootstrap files, private keys, passwords, or API tokens. The
Controller database is a new-generation SQLite store; an existing database
without its generation marker must be replaced with `controller init`.
