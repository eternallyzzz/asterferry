package controller

import "net/http"

const OpenAPISpec = `openapi: 3.0.3
info:
  title: AsterFerry Controller API
  version: 1.0.0
  description: Authoritative control-plane API; data traffic never traverses this service.
servers:
  - url: /api/v1
tags:
  - name: auth
    description: Authentication, users, and API tokens.
  - name: nodes
    description: Enrolled Gateway and Agent identities and observed state.
  - name: resources
    description: Typed desired-state resources.
  - name: operations
    description: Enrollment and runtime operations.
  - name: audit
    description: Immutable audit and event records.
security:
  - bearerAuth: []
  - cookieAuth: []
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: opaque
    cookieAuth:
      type: apiKey
      in: cookie
      name: af_session
  parameters:
    NodeId:
      name: nodeId
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
    IfMatch:
      name: If-Match
      in: header
      required: true
      description: Resource revision used for optimistic concurrency.
      schema: { type: string, pattern: '^[1-9][0-9]*$' }
    IdempotencyKey:
      name: Idempotency-Key
      in: header
      required: false
      schema: { type: string, maxLength: 128 }
    CSRF:
      name: X-CSRF-Token
      in: header
      required: false
      schema: { type: string, minLength: 32, maxLength: 256 }
    Action:
      name: action
      in: path
      required: true
      schema: { type: string, enum: [drain, reconnect, resync] }
    ProxyId:
      name: proxyId
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
    RouteName:
      name: routeName
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
    ServiceId:
      name: serviceId
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
    AssignmentId:
      name: assignmentId
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
    TokenId:
      name: tokenId
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
    UserId:
      name: userId
      in: path
      required: true
      schema: { type: string, minLength: 1, maxLength: 128 }
  responses:
    Unauthorized:
      description: Authentication required.
    Conflict:
      description: Revision, state, or unique-binding conflict.
      content:
        application/json:
          schema: { $ref: '#/components/schemas/ErrorResponse' }
  schemas:
    LoginRequest:
      type: object
      required: [username, password]
      properties:
        username: { type: string, minLength: 1, maxLength: 128 }
        password: { type: string, format: password, minLength: 1, maxLength: 1024 }
    LoginResponse:
      type: object
      properties:
        user: { $ref: '#/components/schemas/User' }
        csrf_token: { type: string }
    User:
      type: object
      properties:
        id: { type: string }
        username: { type: string }
        role: { type: string, enum: [viewer, operator, admin] }
        enabled: { type: boolean }
        revision: { type: integer, format: int64 }
        created_at: { type: string, format: date-time }
        updated_at: { type: string, format: date-time }
    Node:
      type: object
      required: [id, role, name, enabled, certificate_state]
      properties:
        id: { type: string }
        role: { type: string, enum: [gateway, agent] }
        name: { type: string }
        labels: { type: object, additionalProperties: { type: string } }
        enabled: { type: boolean }
        certificate_state: { type: string, enum: [pending, active, revoked, expired] }
        certificate_serial: { type: string }
        revision: { type: integer, format: int64 }
        created_at: { type: string, format: date-time }
        updated_at: { type: string, format: date-time }
    NodeBootstrapRequest:
      type: object
      required: [platform, arch]
      properties:
        platform: { type: string, enum: [linux, windows] }
        arch: { type: string, enum: [amd64, arm64] }
        gateway_spec: { $ref: '#/components/schemas/GatewaySpec' }
        agent_spec: { $ref: '#/components/schemas/AgentSpec' }
    NodeBootstrapResponse:
      type: object
      required: [node_id, role, platform, arch, version, expires_at, command]
      properties:
        installation_id: { type: string, description: Pending installation identifier; set only by the install-first endpoint. }
        state: { type: string, enum: [pending], description: Pending installation state; set only by the install-first endpoint. }
        node_id: { type: string }
        role: { type: string, enum: [gateway, agent] }
        platform: { type: string, enum: [linux, windows] }
        arch: { type: string, enum: [amd64, arm64] }
        version: { type: string, pattern: '^[0-9]+\.[0-9]+\.[0-9]+$' }
        expires_at: { type: string, format: date-time }
        command: { type: string }
    NodeInstallationRequest:
      type: object
      required: [node_id, role, name, platform, arch]
      description: Creates a pending installation intent. It does not create an enrolled node until the installer completes Enroll.
      properties:
        node_id: { type: string, minLength: 1, maxLength: 128 }
        role: { type: string, enum: [gateway, agent] }
        name: { type: string, minLength: 1, maxLength: 256 }
        labels: { type: object, additionalProperties: { type: string } }
        enabled: { type: boolean, default: true }
        platform: { type: string, enum: [linux, windows] }
        arch: { type: string, enum: [amd64, arm64] }
        gateway_spec: { $ref: '#/components/schemas/GatewaySpec' }
        agent_spec: { $ref: '#/components/schemas/AgentSpec' }
    PendingNodeInstallation:
      type: object
      required: [node_id, role, name, enabled, platform, arch, expires_at, created_at]
      properties:
        node_id: { type: string }
        role: { type: string, enum: [gateway, agent] }
        name: { type: string }
        labels: { type: object, additionalProperties: { type: string } }
        enabled: { type: boolean }
        platform: { type: string, enum: [linux, windows] }
        arch: { type: string, enum: [amd64, arm64] }
        expires_at: { type: string, format: date-time }
        created_at: { type: string, format: date-time }
    EgressPolicy:
      type: object
      description: Node-scoped outbound policy. Port ranges use 443 or 8000-8080 syntax.
      properties:
        enabled: { type: boolean }
        tcp_ports:
          type: array
          items: { type: string, pattern: '^[1-9][0-9]{0,4}(-[1-9][0-9]{0,4})?$' }
        udp_ports:
          type: array
          items: { type: string, pattern: '^[1-9][0-9]{0,4}(-[1-9][0-9]{0,4})?$' }
        allow_cidrs: { type: array, items: { type: string } }
        allow_special_cidrs: { type: array, items: { type: string } }
        max_connections: { type: integer, minimum: 0, maximum: 1048576 }
    GatewaySpec:
      type: object
      required: [node_id, public_endpoints]
      properties:
        node_id: { type: string }
        public_endpoints: { type: array, items: { type: string } }
        listeners: { type: array, items: { type: object } }
        labels: { type: object, additionalProperties: { type: string } }
        capacity: { type: object }
        port_pool: { type: object }
        transport: { type: object }
        obfuscation: { type: object }
        egress: { $ref: '#/components/schemas/EgressPolicy' }
        revision: { type: integer, format: int64 }
    AgentSpec:
      type: object
      required: [node_id]
      properties:
        node_id: { type: string }
        gateway_selector: { type: object }
        proxies: { type: array, items: { type: object } }
        routes: { type: array, items: { type: object } }
        limits: { type: object }
        egress: { $ref: '#/components/schemas/EgressPolicy' }
        logging: { type: object }
        revision: { type: integer, format: int64 }
    Service:
      type: object
      required: [id, agent_id, protocol, local_target, public_bind]
      properties:
        id: { type: string }
        agent_id: { type: string }
        protocol: { type: string, enum: [tcp, udp] }
        local_target: { type: string }
        public_bind: { type: string }
        public_port: { type: integer, minimum: 0, maximum: 65535 }
        gateway_selector: { type: object }
        enabled: { type: boolean }
        revision: { type: integer, format: int64 }
        updated_at: { type: string, format: date-time }
    Assignment:
      type: object
      required: [id, gateway_id, agent_id, generation]
      properties:
        id: { type: string }
        gateway_id: { type: string }
        agent_id: { type: string }
        service_ids: { type: array, items: { type: string } }
        bindings: { type: array, items: { type: object } }
        generation: { type: integer, format: int64, minimum: 1 }
        state: { type: string, enum: [pending, applied, degraded, draining] }
        public_endpoint: { type: string }
        revision: { type: integer, format: int64 }
        updated_at: { type: string, format: date-time }
    AssignmentList:
      type: object
      required: [assignments]
      properties:
        assignments: { type: array, items: { $ref: '#/components/schemas/Assignment' } }
    SecretAlreadyCreated:
      type: object
      required: [error, token_recoverable]
      properties:
        error: { type: object, properties: { code: { type: string, enum: [already_created] }, message: { type: string } } }
        token_recoverable: { type: boolean, enum: [false] }
        metadata: { type: object }
        token_metadata: { type: object }
    ErrorResponse:
      type: object
      properties:
        error:
          type: object
          properties:
            code: { type: string }
            message: { type: string }
            path: { type: string }
            retryable: { type: boolean }
paths:
  /healthz: { servers: [{ url: / }], get: { security: [], responses: { "200": { description: Controller health } } } }
  /readyz: { servers: [{ url: / }], get: { responses: { "200": { description: Controller readiness }, "401": { description: Unauthorized }, "503": { description: Database unavailable } } } }
  /metrics: { servers: [{ url: / }], get: { responses: { "200": { description: Prometheus metrics }, "401": { description: Unauthorized } } } }
  /openapi.yaml: { servers: [{ url: / }], get: { security: [], responses: { "200": { description: OpenAPI document } } } }
  /auth/login: { post: { security: [], responses: { "200": { description: OK }, "401": { description: Unauthorized } } } }
  /auth/logout: { post: { responses: { "204": { description: Logged out } } } }
  /me: { get: { responses: { "200": { description: Current user } } } }
  /nodes: { get: { responses: { "200": { description: Nodes } } }, post: { responses: { "201": { description: Created }, "409": { description: Conflict } } } }
  /nodes/{nodeId}: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Node } } }, patch: { responses: { "200": { description: Updated }, "409": { description: Conflict } } }, delete: { responses: { "204": { description: Deleted } } } }
  /nodes/{nodeId}/bootstrap: { parameters: [{ $ref: '#/components/parameters/NodeId' }], post: { requestBody: { required: true, content: { application/json: { schema: { $ref: '#/components/schemas/NodeBootstrapRequest' } } } }, responses: { "201": { description: One-time platform installer command, content: { application/json: { schema: { $ref: '#/components/schemas/NodeBootstrapResponse' } } } }, "409": { description: Token already created }, "503": { description: Bootstrap configuration unavailable } } } }
  /node-installations: { get: { responses: { "200": { description: Pending node installations, content: { application/json: { schema: { type: object, properties: { items: { type: array, items: { $ref: '#/components/schemas/PendingNodeInstallation' } } } } } } } }, post: { requestBody: { required: true, content: { application/json: { schema: { $ref: '#/components/schemas/NodeInstallationRequest' } } } }, responses: { "201": { description: One-time platform installer command; the node is created only after enrollment, content: { application/json: { schema: { $ref: '#/components/schemas/NodeBootstrapResponse' } } } }, "409": { description: Pending installation conflict or unrecoverable token retry }, "503": { description: Bootstrap configuration unavailable } } } }
  /node-installations/{nodeId}: { parameters: [{ $ref: '#/components/parameters/NodeId' }], delete: { responses: { "204": { description: Pending installation cancelled }, "404": { description: Pending installation not found } } } }
  /node-installations/{nodeId}/reissue: { parameters: [{ $ref: '#/components/parameters/NodeId' }], post: { responses: { "200": { description: Replacement one-time installer command, content: { application/json: { schema: { $ref: '#/components/schemas/NodeBootstrapResponse' } } } }, "404": { description: Pending installation not found }, "409": { description: Unrecoverable token retry } } } }
  /nodes/{nodeId}/snapshot: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Desired snapshot } } } }
  /nodes/{nodeId}/desired: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Desired snapshot } } } }
  /nodes/{nodeId}/observed: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Observed state } } } }
  /nodes/{nodeId}/actions/{action}: { parameters: [{ $ref: '#/components/parameters/NodeId' }, { $ref: '#/components/parameters/Action' }], post: { parameters: [{ $ref: '#/components/parameters/IdempotencyKey' }, { $ref: '#/components/parameters/CSRF' }], responses: { "202": { description: Accepted } } } }
  /gateways: { get: { responses: { "200": { description: Gateways } } }, post: { responses: { "201": { description: Created } } } }
  /gateways/{nodeId}: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Gateway } } }, put: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /gateways/{nodeId}/egress: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Gateway egress policy } } }, put: { responses: { "200": { description: Updated policy }, "409": { description: Conflict } } }, patch: { responses: { "200": { description: Updated policy }, "409": { description: Conflict } } } }
  /agents: { get: { responses: { "200": { description: Agents } } }, post: { responses: { "201": { description: Created } } } }
  /agents/{nodeId}: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Agent } } }, put: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /agents/{nodeId}/egress: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Agent egress policy } } }, put: { responses: { "200": { description: Updated policy }, "409": { description: Conflict } } }, patch: { responses: { "200": { description: Updated policy }, "409": { description: Conflict } } } }
  /agents/{nodeId}/actions/schedule: { parameters: [{ $ref: '#/components/parameters/NodeId' }], post: { responses: { "202": { description: Scheduled assignments, content: { application/json: { schema: { $ref: '#/components/schemas/AssignmentList' } } } }, "409": { description: Conflict } } } }
  /agents/{nodeId}/proxies: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Proxy entrances } } }, post: { responses: { "201": { description: Created } } } }
  /agents/{nodeId}/proxies/{proxyId}: { parameters: [{ $ref: '#/components/parameters/NodeId' }, { $ref: '#/components/parameters/ProxyId' }], get: { responses: { "200": { description: Proxy entrance } } }, put: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /agents/{nodeId}/routes: { parameters: [{ $ref: '#/components/parameters/NodeId' }], get: { responses: { "200": { description: Route rules } } }, post: { responses: { "201": { description: Created } } } }
  /agents/{nodeId}/routes/{routeName}: { parameters: [{ $ref: '#/components/parameters/NodeId' }, { $ref: '#/components/parameters/RouteName' }], get: { responses: { "200": { description: Route rule } } }, put: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /services: { get: { responses: { "200": { description: Services } } }, post: { responses: { "201": { description: Created }, "409": { description: Conflict } } } }
  /services/{serviceId}: { parameters: [{ $ref: '#/components/parameters/ServiceId' }], get: { responses: { "200": { description: Service } } }, patch: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /assignments: { get: { responses: { "200": { description: Assignments } } }, post: { responses: { "201": { description: Created } } } }
  /assignments/{assignmentId}: { parameters: [{ $ref: '#/components/parameters/AssignmentId' }], get: { responses: { "200": { description: Assignment } } }, put: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /enrollment-tokens: { get: { responses: { "200": { description: Tokens } } }, post: { responses: { "201": { description: Created }, "409": { description: Already created; plaintext cannot be recovered, content: { application/json: { schema: { $ref: '#/components/schemas/SecretAlreadyCreated' } } } } } } }
  /enrollment-tokens/{tokenId}: { parameters: [{ $ref: '#/components/parameters/TokenId' }], delete: { responses: { "204": { description: Revoked } } } }
  /users: { get: { responses: { "200": { description: Users } } }, post: { responses: { "201": { description: Created } } } }
  /users/{userId}: { parameters: [{ $ref: '#/components/parameters/UserId' }], get: { responses: { "200": { description: User } } }, patch: { responses: { "200": { description: Updated } } }, delete: { responses: { "204": { description: Deleted } } } }
  /users/{userId}/tokens: { parameters: [{ $ref: '#/components/parameters/UserId' }], get: { responses: { "200": { description: Token metadata } } }, post: { responses: { "201": { description: Created }, "409": { description: Already created; plaintext cannot be recovered, content: { application/json: { schema: { $ref: '#/components/schemas/SecretAlreadyCreated' } } } } } } }
  /users/{userId}/tokens/{tokenId}: { parameters: [{ $ref: '#/components/parameters/UserId' }, { $ref: '#/components/parameters/TokenId' }], delete: { responses: { "204": { description: Revoked } } } }
  /audit: { get: { responses: { "200": { description: Audit events } } } }
  /events: { get: { responses: { "200": { description: Events } } } }
`

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(OpenAPISpec))
}
