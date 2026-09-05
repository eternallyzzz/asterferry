// Controller API client. It defaults to the HttpOnly cookie session created
// by /api/v1/auth/login and can optionally send a short-lived API token for
// automation. No node unlock token is ever stored in the Dashboard.

export type ControllerRole = "viewer" | "operator" | "admin";

export interface ControllerUser {
  id: string;
  username: string;
  role: ControllerRole;
  enabled: boolean;
  revision: number;
  created_at?: string;
  updated_at?: string;
}

export interface ControllerNode {
  id: string;
  name: string;
  spec_kind?: "gateway" | "agent";
  labels?: Record<string, string>;
  enabled: boolean;
  certificate_state: string;
  certificate_serial?: string;
  revision: number;
  created_at: string;
  updated_at: string;
}

export type CreateNodeInput = Pick<ControllerNode, "id" | "name" | "labels" | "enabled">;
export interface ControllerNodePatch {
  name?: string;
  labels?: Record<string, string>;
  enabled?: boolean;
  certificate_state?: string;
  certificate_serial?: string;
}

export type NodeSpecKind = "gateway" | "agent";
export interface ControllerSelector {
  match_labels?: Record<string, string>;
}

export interface ControllerListener {
  protocol: string;
  bind: string;
  port: number;
  enabled: boolean;
}

export interface ControllerCapacity {
  max_agents: number;
  max_connections: number;
  max_services: number;
}

export interface ControllerPortRange {
  min: number;
  max: number;
}

export interface ControllerPortPool {
  tcp?: ControllerPortRange[];
  udp?: ControllerPortRange[];
}

export interface ControllerTransportPolicy {
  alpn: string;
  max_streams: number;
  max_frame_bytes: number;
  max_datagram_bytes: number;
  handshake_timeout_seconds: number;
  idle_timeout_seconds: number;
}

// Read models expose only safe obfuscation metadata. Key material is limited
// to the explicit write/editor type so resource summaries cannot render it.
export interface ControllerObfuscationMetadata {
  mode: string;
  key_id?: string;
  previous_key_id?: string;
  max_padding_bytes: number;
  handshake_shaping: boolean;
}

export interface ControllerObfuscationWrite extends ControllerObfuscationMetadata {
  key?: string;
  previous_key?: string;
  key_ciphertext?: string;
  previous_key_ciphertext?: string;
}

export interface ControllerProxySpec {
  id: string;
  protocol: string;
  bind: string;
  route: string;
  enabled: boolean;
}

export interface ControllerRouteRule {
  name: string;
  cidrs?: string[];
  domains?: string[];
  geoip?: string[];
  destination: string;
  enabled: boolean;
}

export interface ControllerAgentLimits {
  max_connections: number;
  max_streams: number;
  max_buffer_bytes: number;
}

export interface ControllerLoggingPolicy {
  level: string;
  format: string;
}

export interface ControllerGatewaySpec {
  node_id: string;
  public_endpoints: string[];
  listeners?: ControllerListener[];
  labels?: Record<string, string>;
  capacity: ControllerCapacity;
  port_pool: ControllerPortPool;
  transport: ControllerTransportPolicy;
  obfuscation: ControllerObfuscationMetadata;
  egress: EgressPolicy;
  revision?: number;
}

export type ControllerGatewaySpecInput = Omit<ControllerGatewaySpec, "obfuscation"> & {
  obfuscation: ControllerObfuscationWrite;
};

export interface ControllerAgentSpec {
  node_id: string;
  gateway_id?: string;
  gateway_selector: ControllerSelector;
  proxies?: ControllerProxySpec[];
  routes?: ControllerRouteRule[];
  limits: ControllerAgentLimits;
  egress: EgressPolicy;
  logging: ControllerLoggingPolicy;
  revision?: number;
}

export type ControllerAgentSpecInput = Omit<ControllerAgentSpec, "gateway_id"> & {
  gateway_id: string;
};

export type ControllerGatewayNodeSpec = {
  node_id: string;
  kind: "gateway";
  gateway: ControllerGatewaySpec;
  revision?: number;
  updated_at?: string;
};

export type ControllerAgentNodeSpec = {
  node_id: string;
  kind: "agent";
  agent: ControllerAgentSpec;
  revision?: number;
  updated_at?: string;
};

export type ControllerNodeSpec = ControllerGatewayNodeSpec | ControllerAgentNodeSpec;

export type ControllerGatewayNodeSpecInput = {
  node_id: string;
  kind: "gateway";
  gateway: ControllerGatewaySpecInput;
};

export type ControllerAgentNodeSpecInput = {
  node_id: string;
  kind: "agent";
  agent: ControllerAgentSpecInput;
};

export type ControllerNodeSpecInput = ControllerGatewayNodeSpecInput | ControllerAgentNodeSpecInput;

export interface ControllerService {
  id: string;
  agent_id: string;
  protocol: "tcp" | "udp";
  local_target: string;
  public_bind: string;
  public_port: number;
  gateway_selector?: ControllerSelector;
  enabled: boolean;
  revision: number;
  updated_at: string;
}

export type ControllerServiceInput = Omit<ControllerService, "revision" | "updated_at">;

export interface ControllerAssignment {
  id: string;
  gateway_id: string;
  agent_id: string;
  service_ids: string[];
  bindings?: Array<{ service_id: string; protocol: "tcp" | "udp"; bind: string; port: number }>;
  generation: number;
  revision?: number;
  state: string;
  public_endpoint?: string;
  obfuscation?: ControllerObfuscationMetadata;
  updated_at: string;
}

// Assignments are scheduler-owned operational records. Their obfuscation
// material is intentionally not part of the Dashboard write model; key
// rotation is performed through the node spec editor.
export type ControllerAssignmentInput = Omit<ControllerAssignment, "revision" | "updated_at" | "obfuscation">;

export interface ControllerApplyError {
  code: string;
  path?: string;
  message: string;
  retryable: boolean;
}

// Legacy names remain exported for the existing agent subresource components.
export interface ProxySpec extends ControllerProxySpec {}
export interface RouteRule extends ControllerRouteRule {}

export interface NodeBootstrapRequest {
  platform: "linux" | "windows";
  arch: "amd64" | "arm64";
}

export interface NodeInstallationRequest extends NodeBootstrapRequest {
  node_id: string;
  name: string;
  labels?: Record<string, string>;
  enabled?: boolean;
}

export interface NodeBootstrapResponse {
  installation_id?: string;
  state?: string;
  node_id: string;
  platform: "linux" | "windows";
  arch: "amd64" | "arm64";
  version: string;
  expires_at: string;
  command: string;
}

export interface PendingNodeInstallation {
  node_id: string;
  name: string;
  labels?: Record<string, string>;
  enabled: boolean;
  platform: "linux" | "windows";
  arch: "amd64" | "arm64";
  expires_at: string;
  created_at: string;
}

export interface ControllerAuditRecord {
  id: number;
  actor: string;
  action: string;
  resource: string;
  resource_id: string;
  revision: number;
  attributes?: Record<string, string>;
  created_at: string;
}

export class ControllerAPIError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "ControllerAPIError";
    this.status = status;
    this.code = code;
  }
}

// Keep a dashboard request from waiting forever when the Controller is down,
// a reverse proxy is misconfigured, or the local HTTPS endpoint is still
// starting.  The browser-side retry/refresh loop can then surface a bounded,
// actionable error instead of leaving the page in a permanent loading state.
export const controllerRequestTimeoutMs = 15_000;

async function request<T>(path: string, init: RequestInit = {}, token?: string): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (token) headers.set("Authorization", "Bearer " + token);
  const csrf = document.cookie.split(";").map((part) => part.trim()).find((part) => part.startsWith("af_csrf="))?.slice("af_csrf=".length);
  if (csrf && init.method && init.method !== "GET" && init.method !== "HEAD") headers.set("X-CSRF-Token", decodeURIComponent(csrf));
  const controller = new AbortController();
  let timedOut = false;
  const timeout = window.setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, controllerRequestTimeoutMs);
  const callerSignal = init.signal;
  const forwardAbort = () => controller.abort(callerSignal?.reason);
  if (callerSignal) {
    if (callerSignal.aborted) forwardAbort();
    else callerSignal.addEventListener("abort", forwardAbort, { once: true });
  }
  try {
    const response = await fetch("/api/v1" + path, { ...init, headers, credentials: "include", cache: "no-store", signal: controller.signal });
    if (!response.ok) {
      let message = "request failed with HTTP " + response.status;
      let code: string | undefined;
      try {
        const body = (await response.json()) as { error?: { code?: string; message?: string } };
        message = body.error?.message || message;
        code = body.error?.code;
      } catch {
        // Keep the stable HTTP status when the response is not JSON.
      }
      throw new ControllerAPIError(response.status, message, code);
    }
    if (response.status === 204) return undefined as T;
    // Keep the timeout active through body consumption as well as header
    // delivery. A proxy can accept a request and then stall its response body.
    return (await response.json()) as T;
  } catch (error) {
    if (timedOut) {
      throw new ControllerAPIError(0, "Controller 请求超时，请检查 Controller 是否在线。", "request_timeout");
    }
    throw error;
  } finally {
    window.clearTimeout(timeout);
    callerSignal?.removeEventListener("abort", forwardAbort);
  }
}

export function login(username: string, password: string): Promise<{ user: ControllerUser; csrf_token: string }> {
  return request("/auth/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username, password }) });
}

export function logout(): Promise<void> { return request<void>("/auth/logout", { method: "POST" }); }
export function currentUser(token?: string): Promise<ControllerUser> { return request<ControllerUser>("/me", {}, token); }
export function listNodes(kind?: NodeSpecKind, token?: string): Promise<{ items: ControllerNode[] }> { return request(`/nodes${kind ? `?kind=${encodeURIComponent(kind)}` : ""}`, {}, token); }
export function getNode(id: string, token?: string): Promise<ControllerNode> { return request<ControllerNode>(`/nodes/${encodeURIComponent(id)}`, {}, token); }
export function createNode(node: CreateNodeInput, token?: string, idempotencyKey?: string): Promise<ControllerNode> { return request<ControllerNode>("/nodes", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(node) }, token); }
export function updateNode(id: string, node: ControllerNodePatch, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerNode> { return request<ControllerNode>(`/nodes/${encodeURIComponent(id)}`, { method: "PATCH", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(node) }, token); }
export function deleteNode(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/nodes/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function bootstrapNode(id: string, input: NodeBootstrapRequest, token?: string, idempotencyKey?: string): Promise<NodeBootstrapResponse> { return request<NodeBootstrapResponse>(`/nodes/${encodeURIComponent(id)}/bootstrap`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(input) }, token); }
export function listNodeInstallations(token?: string): Promise<{ items: PendingNodeInstallation[] }> { return request<{ items: PendingNodeInstallation[] }>("/node-installations", {}, token); }
export function createNodeInstallation(input: NodeInstallationRequest, token?: string, idempotencyKey?: string): Promise<NodeBootstrapResponse> { return request<NodeBootstrapResponse>("/node-installations", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(input) }, token); }
export function reissueNodeInstallation(id: string, token?: string, idempotencyKey?: string): Promise<NodeBootstrapResponse> { return request<NodeBootstrapResponse>(`/node-installations/${encodeURIComponent(id)}/reissue`, { method: "POST", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function deleteNodeInstallation(id: string, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/node-installations/${encodeURIComponent(id)}`, { method: "DELETE", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function nodeAction(id: string, action: "drain" | "reconnect" | "resync", token?: string, idempotencyKey?: string): Promise<{ state: string }> { return request<{ state: string }>(`/nodes/${encodeURIComponent(id)}/actions/${action}`, { method: "POST", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function getNodeSpec(id: string, token?: string): Promise<ControllerNodeSpec> { return request<ControllerNodeSpec>(`/nodes/${encodeURIComponent(id)}/spec`, {}, token); }
export function putNodeSpec(id: string, spec: ControllerNodeSpecInput, revision?: number, token?: string, idempotencyKey?: string): Promise<ControllerNodeSpec> { return request<ControllerNodeSpec>(`/nodes/${encodeURIComponent(id)}/spec`, { method: "PUT", headers: { "Content-Type": "application/json", ...(revision !== undefined ? { "If-Match": String(revision) } : {}), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(spec) }, token); }
export function deleteNodeSpec(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/nodes/${encodeURIComponent(id)}/spec`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function getNodeEgress(id: string, token?: string): Promise<EgressPolicy> { return request(`/nodes/${encodeURIComponent(id)}/spec/egress`, {}, token); }
export function updateNodeEgress(id: string, policy: EgressPolicy, revision: number, token?: string, idempotencyKey?: string): Promise<EgressPolicy> { return request<EgressPolicy>(`/nodes/${encodeURIComponent(id)}/spec/egress`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(policy) }, token); }
export function scheduleNode(id: string, token?: string, idempotencyKey?: string): Promise<{ assignments: ControllerAssignment[] }> { return request<{ assignments: ControllerAssignment[] }>(`/nodes/${encodeURIComponent(id)}/actions/schedule`, { method: "POST", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function listServices(agentID?: string, token?: string): Promise<{ items: ControllerService[] }> { return request(`/services${agentID ? `?agent_id=${encodeURIComponent(agentID)}` : ""}`, {}, token); }
export function createService(service: ControllerServiceInput, token?: string, idempotencyKey?: string): Promise<ControllerService> { return request<ControllerService>("/services", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(service) }, token); }
export function updateService(id: string, service: Partial<ControllerServiceInput>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerService> { return request<ControllerService>(`/services/${encodeURIComponent(id)}`, { method: "PATCH", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(service) }, token); }
export function deleteService(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/services/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function listAssignments(token?: string): Promise<{ items: ControllerAssignment[] }> { return request<{ items: ControllerAssignment[] }>("/assignments", {}, token); }
export function createAssignment(assignment: ControllerAssignmentInput, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>("/assignments", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(assignment) }, token); }
export function getAssignment(id: string, token?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>(`/assignments/${encodeURIComponent(id)}`, {}, token); }
export function updateAssignment(id: string, assignment: Partial<ControllerAssignmentInput>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>(`/assignments/${encodeURIComponent(id)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(assignment) }, token); }
export function deleteAssignment(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/assignments/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function listAudit(limit = 100, token?: string): Promise<{ items: ControllerAuditRecord[] }> { return request<{ items: ControllerAuditRecord[] }>(`/audit?limit=${limit}`, {}, token); }
// 与后端 EnrollmentToken 对齐：used_at 在未使用/未吊销时整个字段被省略（omitempty）。
export interface EnrollmentTokenMeta {
  id: string;

  expires_at: string;
  used_at?: string;
  created_at: string;
}

// 与后端 APIToken 对齐：expires_at/revoked_at 可省略；不存在 last_used_at。
export interface APITokenMeta {
  id: string;
  user_id: string;
  name: string;
  expires_at?: string;
  revoked_at?: string;
  created_at: string;
}

export function createEnrollmentToken(ttlSeconds?: number, token?: string, idempotencyKey?: string): Promise<{ token: string; token_metadata: EnrollmentTokenMeta; created_by?: string }> { return request(`/enrollment-tokens`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify({ ttl_seconds: ttlSeconds }) }, token); }
export function listEnrollmentTokens(token?: string): Promise<{ items: EnrollmentTokenMeta[] }> { return request(`/enrollment-tokens`, {}, token); }
export function revokeEnrollmentToken(id: string, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/enrollment-tokens/${encodeURIComponent(id)}`, { method: "DELETE", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function listUserTokens(userID: string, token?: string): Promise<{ items: APITokenMeta[] }> { return request(`/users/${encodeURIComponent(userID)}/tokens`, {}, token); }
export function createUserToken(userID: string, name: string, expiresAt?: string, token?: string, idempotencyKey?: string): Promise<{ token: string; metadata: APITokenMeta; created_by?: string }> { return request(`/users/${encodeURIComponent(userID)}/tokens`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify({ name, expires_at: expiresAt ?? null }) }, token); }
export function revokeUserToken(userID: string, tokenID: string, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/users/${encodeURIComponent(userID)}/tokens/${encodeURIComponent(tokenID)}`, { method: "DELETE", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function listUsers(token?: string): Promise<{ items: ControllerUser[] }> { return request<{ items: ControllerUser[] }>("/users", {}, token); }
export function createUser(input: { username: string; password: string; role: ControllerRole }, token?: string, idempotencyKey?: string): Promise<ControllerUser> { return request<ControllerUser>("/users", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(input) }, token); }
export function updateUser(id: string, input: Partial<Pick<ControllerUser, "username" | "role" | "enabled">> & { password?: string }, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerUser> { return request<ControllerUser>(`/users/${encodeURIComponent(id)}`, { method: "PATCH", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(input) }, token); }
export function deleteUser(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/users/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }

// ---------------------------------------------------------------------------
// 事件、观测状态与快照
// ---------------------------------------------------------------------------

// 事件与审计同源：事件为 action 以 "event:" 为前缀、resource 为 "event" 的记录，
// 前端按此约定做分段筛选。
export function listEvents(limit = 100, token?: string): Promise<{ items: ControllerAuditRecord[] }> { return request<{ items: ControllerAuditRecord[] }>(`/events?limit=${limit}`, {}, token); }

export interface ControllerSessionSummary {
  id: string;
  peer_id: string;
  started_at: string;
  streams: number;
}

export interface ControllerListenerState {
  protocol: string;
  bind: string;
  port: number;
  ready: boolean;
}

export interface ControllerRuntimeMetrics {
  active_streams?: number;
  active_sessions?: number;
  active_egress?: number;
  udp_oversize_drops?: number;
  geoip_up?: boolean;
  active_connections?: number;
  active_flows?: number;
  runtime_bytes_in_total?: number;
  runtime_bytes_out_total?: number;
  runtime_opened_total?: number;
  runtime_closed_total?: number;
  runtime_rejected_total?: number;
  runtime_rate_limited_total?: number;
  runtime_telemetry_dropped_total?: number;
}

export interface ControllerObservedState {
  schema_version: number;
  node_id: string;
  applied_generation: number;
  healthy: boolean;
  degraded: boolean;
  last_error?: ControllerApplyError;
  sessions?: ControllerSessionSummary[];
  listeners?: ControllerListenerState[];
  metrics?: ControllerRuntimeMetrics;
  observed_at: string;
}

export interface ControllerSnapshot {
  schema_version: number;
  node_id: string;
  generation: number;
  checksum: string;
  gateway?: ControllerGatewaySpec;
  agent?: ControllerAgentSpec;
  services?: ControllerService[];
  assignments?: ControllerAssignment[];
}

export function getObserved(id: string, token?: string): Promise<ControllerObservedState> { return request<ControllerObservedState>(`/nodes/${encodeURIComponent(id)}/observed`, {}, token); }
export function getSnapshot(id: string, token?: string): Promise<ControllerSnapshot> { return request<ControllerSnapshot>(`/nodes/${encodeURIComponent(id)}/snapshot`, {}, token); }

// ---------------------------------------------------------------------------
// 规格删除与 Egress 策略（If-Match 为父规格文档的 revision）
// ---------------------------------------------------------------------------

export interface EgressPolicy {
  enabled: boolean;
  tcp_ports?: string[];
  udp_ports?: string[];
  allow_cidrs?: string[];
  allow_special_cidrs?: string[];
  max_connections: number;
}


// ---------------------------------------------------------------------------
// Node Agent proxies / routes. These are CAS subresources of the unified
// node spec and are available only when the node's persisted kind is agent.

export interface RuntimeRateLimit {
  direction: "in" | "out" | "both";
  bytes_per_second: number;
  burst_bytes: number;
  expires_at: string;
}

export interface RuntimeConnection {
  id: string;
  type: "session" | "tcp" | "udp_flow" | "egress" | string;
  node_id: string;
  peer_node_id?: string;
  gateway_id?: string;
  agent_id?: string;
  assignment_id?: string;
  service_id?: string;
  protocol: string;
  source_ip?: string;
  source_port?: number;
  target?: string;
  parent_session_id?: string;
  started_at: string;
  last_activity_at: string;
  ended_at?: string;
  state: "active" | "closed" | "unknown" | string;
  close_reason?: string;
  bytes_in: number;
  bytes_out: number;
  rate_in_bps: number;
  rate_out_bps: number;
  limit?: RuntimeRateLimit;
}

export interface RuntimeTrafficRollup {
  bucket_start: string;
  node_id: string;
  gateway_id?: string;
  agent_id?: string;
  assignment_id?: string;
  service_id?: string;
  protocol: string;
  bytes_in: number;
  bytes_out: number;
  opened: number;
  closed: number;
  rejected: number;
  rate_limited: number;
  active_max: number;
}

export interface RuntimeEventRecord {
  id: number;
  event_id: string;
  node_id: string;
  connection_id?: string;
  type: string;
  payload: Record<string, unknown>;
  created_at: string;
}

export interface RuntimeSettings {
  advanced_operations_enabled: boolean;
  runtime_retention_days: number;
}

export function listRuntimeConnections(nodeID?: string, query = "", token?: string): Promise<{ items: RuntimeConnection[] }> {
  const params = new URLSearchParams(query);
  if (nodeID) params.set("node_id", nodeID);
  const suffix = params.toString() ? `?${params.toString()}` : "";
  return request<{ items: RuntimeConnection[] }>(`/runtime/connections${suffix}`, {}, token);
}
export function getNodeRuntimeConnections(nodeID: string, token?: string): Promise<{ items: RuntimeConnection[] }> { return listRuntimeConnections(nodeID, "limit=500", token); }
export function getNodeRuntimeConnection(nodeID: string, connectionID: string, token?: string): Promise<RuntimeConnection> { return request<RuntimeConnection>(`/nodes/${encodeURIComponent(nodeID)}/runtime/connections/${encodeURIComponent(connectionID)}`, {}, token); }
export function listRuntimeTraffic(nodeID?: string, token?: string): Promise<{ items: RuntimeTrafficRollup[] }> { return request(`/runtime/traffic${nodeID ? `?node_id=${encodeURIComponent(nodeID)}` : ""}`, {}, token); }
export function listRuntimeEvents(nodeID?: string, token?: string, limit = 100): Promise<{ items: RuntimeEventRecord[] }> { const params = new URLSearchParams({ limit: String(limit) }); if (nodeID) params.set("node_id", nodeID); return request(`/runtime/events?${params.toString()}`, {}, token); }
export function getRuntimeSettings(token?: string): Promise<RuntimeSettings> { return request<RuntimeSettings>("/runtime/settings", {}, token); }
export function setRuntimeSettings(enabled: boolean, token?: string, idempotencyKey?: string): Promise<RuntimeSettings> { return request<RuntimeSettings>("/runtime/settings", { method: "PUT", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify({ advanced_operations_enabled: enabled }) }, token); }
export interface RuntimeActionInput {
  action: "disconnect" | "rate_limit" | "clear_limit";
  selector?: { connection_id?: string; source_ip?: string; peer_node_id?: string; assignment_id?: string; service_id?: string; protocol?: string };
  direction?: "in" | "out" | "both";
  bytes_per_second?: number;
  burst_bytes?: number;
  ttl_seconds?: number;
}
export function runtimeAction(nodeID: string, input: RuntimeActionInput, token?: string, idempotencyKey?: string): Promise<{ node_id: string; action: string; state: string }> { return request(`/nodes/${encodeURIComponent(nodeID)}/runtime/actions`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(input) }, token); }
export function runtimeConnectionAction(nodeID: string, connectionID: string, input: Omit<RuntimeActionInput, "selector">, token?: string, idempotencyKey?: string): Promise<{ node_id: string; action: string; state: string }> { return request(`/nodes/${encodeURIComponent(nodeID)}/runtime/connections/${encodeURIComponent(connectionID)}/actions`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(input) }, token); }
export function runtimeStreamURL(nodeID?: string): string { return `/api/v1/runtime/stream${nodeID ? `?node_id=${encodeURIComponent(nodeID)}` : ""}`; }

export async function listNodeProxies(id: string, token?: string): Promise<{ items: ProxySpec[] }> {
  const result = await request<{ items: ProxySpec[] | null }>(`/nodes/${encodeURIComponent(id)}/spec/proxies`, {}, token);
  return { items: result.items ?? [] };
}
export function createNodeProxy(id: string, proxy: ProxySpec, specRevision: number, token?: string, idempotencyKey?: string): Promise<ProxySpec> { return request<ProxySpec>(`/nodes/${encodeURIComponent(id)}/spec/proxies`, { method: "POST", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(proxy) }, token); }
export function updateNodeProxy(id: string, proxyId: string, proxy: ProxySpec, specRevision: number, token?: string, idempotencyKey?: string): Promise<ProxySpec> { return request<ProxySpec>(`/nodes/${encodeURIComponent(id)}/spec/proxies/${encodeURIComponent(proxyId)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(proxy) }, token); }
export function deleteNodeProxy(id: string, proxyId: string, specRevision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/nodes/${encodeURIComponent(id)}/spec/proxies/${encodeURIComponent(proxyId)}`, { method: "DELETE", headers: { "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }

export async function listNodeRoutes(id: string, token?: string): Promise<{ items: RouteRule[] }> {
  const result = await request<{ items: RouteRule[] | null }>(`/nodes/${encodeURIComponent(id)}/spec/routes`, {}, token);
  return { items: result.items ?? [] };
}
export function createNodeRoute(id: string, route: RouteRule, specRevision: number, token?: string, idempotencyKey?: string): Promise<RouteRule> { return request<RouteRule>(`/nodes/${encodeURIComponent(id)}/spec/routes`, { method: "POST", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(route) }, token); }
export function updateNodeRoute(id: string, routeName: string, route: RouteRule, specRevision: number, token?: string, idempotencyKey?: string): Promise<RouteRule> { return request<RouteRule>(`/nodes/${encodeURIComponent(id)}/spec/routes/${encodeURIComponent(routeName)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(route) }, token); }
export function deleteNodeRoute(id: string, routeName: string, specRevision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/nodes/${encodeURIComponent(id)}/spec/routes/${encodeURIComponent(routeName)}`, { method: "DELETE", headers: { "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
