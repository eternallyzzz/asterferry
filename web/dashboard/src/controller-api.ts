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
  role: "gateway" | "agent";
  name: string;
  labels?: Record<string, string>;
  enabled: boolean;
  certificate_state: string;
  revision: number;
  created_at: string;
  updated_at: string;
}

export type CreateNodeInput = Pick<ControllerNode, "id" | "role" | "name" | "labels" | "enabled">;

export interface ControllerService {
  id: string;
  agent_id: string;
  protocol: "tcp" | "udp";
  local_target: string;
  public_bind: string;
  public_port: number;
  gateway_selector?: { match_labels?: Record<string, string> };
  enabled: boolean;
  revision: number;
}

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
export function listNodes(role?: ControllerNode["role"], token?: string): Promise<{ items: ControllerNode[] }> { return request(`/nodes${role ? `?role=${encodeURIComponent(role)}` : ""}`, {}, token); }
export function getNode(id: string, token?: string): Promise<ControllerNode> { return request<ControllerNode>(`/nodes/${encodeURIComponent(id)}`, {}, token); }
export function createNode(node: CreateNodeInput, token?: string, idempotencyKey?: string): Promise<ControllerNode> { return request<ControllerNode>("/nodes", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(node) }, token); }
export function updateNode(id: string, node: Partial<ControllerNode>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerNode> { return request<ControllerNode>(`/nodes/${encodeURIComponent(id)}`, { method: "PATCH", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(node) }, token); }
export function deleteNode(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/nodes/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function nodeAction(id: string, action: "drain" | "reconnect" | "resync", token?: string, idempotencyKey?: string): Promise<{ state: string }> { return request<{ state: string }>(`/nodes/${encodeURIComponent(id)}/actions/${action}`, { method: "POST", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function listGateways(token?: string): Promise<{ items: Array<{ node: ControllerNode; spec?: unknown }> }> { return request(`/gateways`, {}, token); }
export function createGateway(spec: unknown, token?: string, idempotencyKey?: string): Promise<unknown> { return request(`/gateways`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(spec) }, token); }
export function getGateway(id: string, token?: string): Promise<unknown> { return request(`/gateways/${encodeURIComponent(id)}`, {}, token); }
export function updateGateway(id: string, spec: unknown, revision: number, token?: string, idempotencyKey?: string): Promise<unknown> { return request(`/gateways/${encodeURIComponent(id)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(spec) }, token); }
export function getGatewayEgress(id: string, token?: string): Promise<EgressPolicy> { return request(`/gateways/${encodeURIComponent(id)}/egress`, {}, token); }
export function updateGatewayEgress(id: string, policy: EgressPolicy, revision: number, token?: string, idempotencyKey?: string): Promise<EgressPolicy> { return request(`/gateways/${encodeURIComponent(id)}/egress`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(policy) }, token); }
export function listAgents(token?: string): Promise<{ items: Array<{ node: ControllerNode; spec?: unknown }> }> { return request(`/agents`, {}, token); }
export function createAgent(spec: unknown, token?: string, idempotencyKey?: string): Promise<unknown> { return request(`/agents`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(spec) }, token); }
export function getAgent(id: string, token?: string): Promise<unknown> { return request(`/agents/${encodeURIComponent(id)}`, {}, token); }
export function updateAgent(id: string, spec: unknown, revision: number, token?: string, idempotencyKey?: string): Promise<unknown> { return request(`/agents/${encodeURIComponent(id)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(spec) }, token); }
export function getAgentEgress(id: string, token?: string): Promise<EgressPolicy> { return request(`/agents/${encodeURIComponent(id)}/egress`, {}, token); }
export function updateAgentEgress(id: string, policy: EgressPolicy, revision: number, token?: string, idempotencyKey?: string): Promise<EgressPolicy> { return request(`/agents/${encodeURIComponent(id)}/egress`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(policy) }, token); }
export function scheduleAgent(id: string, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>(`/agents/${encodeURIComponent(id)}/actions/schedule`, { method: "POST", headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : undefined }, token); }
export function listServices(agentID?: string, token?: string): Promise<{ items: ControllerService[] }> { return request(`/services${agentID ? `?agent_id=${encodeURIComponent(agentID)}` : ""}`, {}, token); }
export function createService(service: Omit<ControllerService, "revision">, token?: string, idempotencyKey?: string): Promise<ControllerService> { return request<ControllerService>("/services", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(service) }, token); }
export function updateService(id: string, service: Partial<ControllerService>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerService> { return request<ControllerService>(`/services/${encodeURIComponent(id)}`, { method: "PATCH", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(service) }, token); }
export function deleteService(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/services/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function listAssignments(token?: string): Promise<{ items: ControllerAssignment[] }> { return request<{ items: ControllerAssignment[] }>("/assignments", {}, token); }
export function createAssignment(assignment: Omit<ControllerAssignment, "revision">, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>("/assignments", { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(assignment) }, token); }
export function getAssignment(id: string, token?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>(`/assignments/${encodeURIComponent(id)}`, {}, token); }
export function updateAssignment(id: string, assignment: Partial<ControllerAssignment>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> { return request<ControllerAssignment>(`/assignments/${encodeURIComponent(id)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(assignment) }, token); }
export function deleteAssignment(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/assignments/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function listAudit(limit = 100, token?: string): Promise<{ items: ControllerAuditRecord[] }> { return request<{ items: ControllerAuditRecord[] }>(`/audit?limit=${limit}`, {}, token); }
// 与后端 EnrollmentToken 对齐：used_at 在未使用/未吊销时整个字段被省略（omitempty）。
export interface EnrollmentTokenMeta {
  id: string;
  role: "gateway" | "agent";
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

export function createEnrollmentToken(role: "gateway" | "agent", ttlSeconds?: number, token?: string, idempotencyKey?: string): Promise<{ token: string; token_metadata: EnrollmentTokenMeta; created_by?: string }> { return request(`/enrollment-tokens`, { method: "POST", headers: { "Content-Type": "application/json", ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify({ role, ttl_seconds: ttlSeconds }) }, token); }
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

export interface ControllerObservedState {
  schema_version: number;
  node_id: string;
  applied_generation: number;
  healthy: boolean;
  degraded: boolean;
  last_error?: { code: string; path: string; message: string };
  sessions?: ControllerSessionSummary[];
  listeners?: ControllerListenerState[];
  metrics?: Record<string, number>;
  observed_at: string;
}

export interface ControllerSnapshot {
  schema_version: number;
  node_id: string;
  generation: number;
  checksum: string;
  gateway?: unknown;
  agent?: unknown;
  services?: unknown[];
  assignments?: unknown[];
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

export function deleteGateway(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/gateways/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
export function deleteAgent(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/agents/${encodeURIComponent(id)}`, { method: "DELETE", headers: { "If-Match": String(revision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }

// ---------------------------------------------------------------------------
// Agent proxies / routes（AgentSpec 的 CAS 子资源，变更需携带父规格 revision）
// ---------------------------------------------------------------------------

export interface ProxySpec {
  id: string;
  protocol: string;
  bind: string;
  route: string;
  enabled: boolean;
}

export interface RouteRule {
  name: string;
  cidrs?: string[];
  domains?: string[];
  geoip?: string[];
  destination: string;
  enabled: boolean;
}

// 后端对空集合返回 items: null，统一归一化为空数组。
export async function listAgentProxies(id: string, token?: string): Promise<{ items: ProxySpec[] }> {
  const result = await request<{ items: ProxySpec[] | null }>(`/agents/${encodeURIComponent(id)}/proxies`, {}, token);
  return { items: result.items ?? [] };
}
export function createAgentProxy(id: string, proxy: ProxySpec, specRevision: number, token?: string, idempotencyKey?: string): Promise<ProxySpec> { return request<ProxySpec>(`/agents/${encodeURIComponent(id)}/proxies`, { method: "POST", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(proxy) }, token); }
export function updateAgentProxy(id: string, proxyId: string, proxy: ProxySpec, specRevision: number, token?: string, idempotencyKey?: string): Promise<ProxySpec> { return request<ProxySpec>(`/agents/${encodeURIComponent(id)}/proxies/${encodeURIComponent(proxyId)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(proxy) }, token); }
export function deleteAgentProxy(id: string, proxyId: string, specRevision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/agents/${encodeURIComponent(id)}/proxies/${encodeURIComponent(proxyId)}`, { method: "DELETE", headers: { "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }

export async function listAgentRoutes(id: string, token?: string): Promise<{ items: RouteRule[] }> {
  const result = await request<{ items: RouteRule[] | null }>(`/agents/${encodeURIComponent(id)}/routes`, {}, token);
  return { items: result.items ?? [] };
}
export function createAgentRoute(id: string, route: RouteRule, specRevision: number, token?: string, idempotencyKey?: string): Promise<RouteRule> { return request<RouteRule>(`/agents/${encodeURIComponent(id)}/routes`, { method: "POST", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(route) }, token); }
export function updateAgentRoute(id: string, routeName: string, route: RouteRule, specRevision: number, token?: string, idempotencyKey?: string): Promise<RouteRule> { return request<RouteRule>(`/agents/${encodeURIComponent(id)}/routes/${encodeURIComponent(routeName)}`, { method: "PUT", headers: { "Content-Type": "application/json", "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) }, body: JSON.stringify(route) }, token); }
export function deleteAgentRoute(id: string, routeName: string, specRevision: number, token?: string, idempotencyKey?: string): Promise<void> { return request<void>(`/agents/${encodeURIComponent(id)}/routes/${encodeURIComponent(routeName)}`, { method: "DELETE", headers: { "If-Match": String(specRevision), ...(idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {}) } }, token); }
