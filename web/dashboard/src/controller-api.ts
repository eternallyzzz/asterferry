import createClient from "openapi-fetch";
import type { components, paths } from "./generated/controller-api";

type Schemas = components["schemas"];

// The generated contract is the only source of response/domain shapes. These
// aliases retain the public names used by the Dashboard without duplicating
// the OpenAPI model in handwritten TypeScript.
export type ControllerRole = NonNullable<Schemas["User"]["role"]>;
export type ControllerUser = Omit<Schemas["User"], "created_at" | "updated_at"> &
  Partial<Pick<Schemas["User"], "created_at" | "updated_at">>;
export type ControllerNode = Schemas["Node"];
export type CreateNodeInput = Pick<Schemas["NodeInput"], "id" | "name" | "labels" | "enabled">;
export type ControllerNodePatch = Schemas["NodePatch"];
export type ControllerUpdateState = Schemas["ControllerUpdateState"];
export type NodeUpdateStatus = Schemas["NodeUpdateStatus"];
export type NodeSpecKind = "gateway" | "agent";
export type ControllerSelector = Schemas["Selector"];
export type ControllerListener = Schemas["Listener"];
export type ControllerCapacity = Schemas["Capacity"];
export type ControllerPortRange = Schemas["PortRange"];
export type ControllerPortPool = Schemas["PortPool"];
export type ControllerTransportPolicy = Schemas["TransportPolicy"];
export type ControllerObfuscationMetadata = Schemas["ObfuscationMetadata"];
export type ControllerObfuscationWrite = Schemas["ObfuscationWrite"];
export type ControllerProxySpec = Schemas["ProxySpec"];
export type ControllerRouteRule = Schemas["RouteRule"];
export type ControllerAgentLimits = Schemas["AgentLimits"];
export type ControllerLoggingPolicy = Schemas["LoggingPolicy"];
export type ControllerGatewaySpec = Schemas["GatewaySpec"];
export type ControllerGatewaySpecInput = Omit<ControllerGatewaySpec, "obfuscation" | "revision"> & {
  obfuscation: ControllerObfuscationWrite;
};
export type ControllerAgentSpec = Schemas["AgentSpec"];
export type ControllerAgentSpecInput = Omit<ControllerAgentSpec, "gateway_id" | "revision"> & {
  gateway_id: string;
};
export type ControllerGatewayNodeSpec = Schemas["GatewayNodeSpec"];
export type ControllerAgentNodeSpec = Schemas["AgentNodeSpec"];
export type ControllerNodeSpec = Schemas["NodeSpec"];
export type ControllerGatewayNodeSpecInput = Omit<ControllerGatewayNodeSpec, "revision" | "updated_at"> & {
  gateway: ControllerGatewaySpecInput;
};
export type ControllerAgentNodeSpecInput = Omit<ControllerAgentNodeSpec, "revision" | "updated_at"> & {
  agent: ControllerAgentSpecInput;
};
export type ControllerNodeSpecInput = ControllerGatewayNodeSpecInput | ControllerAgentNodeSpecInput;
export type ControllerService = Schemas["Service"];
export type ControllerServiceInput = Schemas["ServiceInput"];
export type ControllerAssignment = Schemas["Assignment"];
export type ControllerAssignmentInput = Schemas["AssignmentInput"];
export type ControllerApplyError = Schemas["ErrorDetail"];
export type ProxySpec = ControllerProxySpec;
export type RouteRule = ControllerRouteRule;
export type NodeInstallScriptSource = Schemas["NodeBootstrapRequest"]["script_source"];
export type NodeBootstrapRequest = Schemas["NodeBootstrapRequest"];
export type ControllerSystemInfo = Schemas["SystemInfo"];
export type NodeInstallationRequest = Schemas["NodeInstallationRequest"];
export type NodeBootstrapResponse = Schemas["NodeBootstrapResponse"];
export type PendingNodeInstallation = Schemas["PendingNodeInstallation"];
export type ControllerAuditRecord = Schemas["AuditRecord"];
export type EnrollmentTokenMeta = Schemas["EnrollmentTokenMeta"];
export type APITokenMeta = Schemas["APITokenMeta"];
export type ControllerSessionSummary = Schemas["SessionSummary"];
export type ControllerListenerState = Schemas["ListenerState"];
export type ControllerRuntimeMetrics = Schemas["RuntimeMetrics"];
export type ControllerObservedState = Schemas["ObservedState"];
export type ControllerSnapshot = Schemas["DesiredSnapshot"];
export type EgressPolicy = Schemas["EgressPolicy"];
export type RuntimeRateLimit = Schemas["RuntimeRateLimit"];
export type RuntimeConnection = Schemas["RuntimeConnection"];
export type RuntimeTrafficRollup = Schemas["RuntimeTrafficRollup"];
export type RuntimeEventRecord = Schemas["RuntimeEventRecord"];
export type RuntimeSettings = Schemas["RuntimeSettings"];
export type RuntimeActionInput = Schemas["RuntimeAction"];
export type RuntimeActionResponse = Schemas["RuntimeActionResponse"];

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

export const controllerRequestTimeoutMs = 15_000;

class ControllerRequestTimeout extends Error {
  constructor() {
    super("Controller request timed out");
    this.name = "ControllerRequestTimeout";
  }
}

function csrfToken(): string | undefined {
  if (typeof document === "undefined") return undefined;
  const value = document.cookie
    .split(";")
    .map((part) => part.trim())
    .find((part) => part.startsWith("af_csrf="))
    ?.slice("af_csrf=".length);
  return value ? decodeURIComponent(value) : undefined;
}

function requestHeaders(method: string, token?: string, revision?: number, idempotencyKey?: string): Record<string, string> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token) headers.Authorization = "Bearer " + token;
  if (revision !== undefined) headers["If-Match"] = String(revision);
  if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
  if (method !== "GET" && method !== "HEAD") {
    const csrf = csrfToken();
    if (csrf) headers["X-CSRF-Token"] = csrf;
  }
  return headers;
}

async function fetchWithTimeout(input: Request): Promise<Response> {
  const controller = new AbortController();
  let timedOut = false;
  const timeout = globalThis.setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, controllerRequestTimeoutMs);
  const forwardAbort = () => controller.abort();
  if (input.signal.aborted) forwardAbort();
  else input.signal.addEventListener("abort", forwardAbort, { once: true });
  try {
    return await globalThis.fetch(new Request(input, { signal: controller.signal, cache: "no-store" }));
  } catch (error) {
    if (timedOut) throw new ControllerRequestTimeout();
    throw error;
  } finally {
    globalThis.clearTimeout(timeout);
    input.signal.removeEventListener("abort", forwardAbort);
  }
}

const client = createClient<paths>({
  baseUrl: "/api/v1",
  credentials: "include",
  cache: "no-store",
  fetch: fetchWithTimeout,
});

type ClientResult<T> = { data?: T; error?: unknown; response: Response };

function controllerError(status: number, error: unknown): ControllerAPIError {
  const body = error && typeof error === "object" ? error as { error?: { code?: string; message?: string } } : undefined;
  const detail = body?.error;
  return new ControllerAPIError(status, detail?.message || "request failed with HTTP " + status, detail?.code);
}

async function execute<T>(pending: Promise<ClientResult<T>>): Promise<T> {
  try {
    const result = await pending;
    if (!result.response.ok || result.error !== undefined) throw controllerError(result.response.status, result.error);
    return result.data as T;
  } catch (error) {
    if (error instanceof ControllerRequestTimeout) {
      throw new ControllerAPIError(0, "Controller 请求超时，请检查 Controller 是否在线。", "request_timeout");
    }
    throw error;
  }
}

export function login(username: string, password: string): Promise<{ user: ControllerUser; csrf_token: string }> {
  return execute(client.POST("/auth/login", { headers: requestHeaders("POST"), body: { username, password } }));
}

export function logout(): Promise<void> {
  return execute(client.POST("/auth/logout", { headers: requestHeaders("POST") }));
}

export function currentUser(token?: string): Promise<ControllerUser> {
  return execute(client.GET("/me", { headers: requestHeaders("GET", token) }));
}

export function listNodes(kind?: NodeSpecKind, token?: string): Promise<{ items: ControllerNode[] }> {
  return execute(client.GET("/nodes", { headers: requestHeaders("GET", token), params: { query: kind ? { kind } : {} } }));
}

export function getNode(id: string, token?: string): Promise<ControllerNode> {
  return execute(client.GET("/nodes/{nodeId}", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
}

export function createNode(node: CreateNodeInput, token?: string, idempotencyKey?: string): Promise<ControllerNode> {
  return execute(client.POST("/nodes", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: node }));
}

export function updateNode(id: string, node: ControllerNodePatch, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerNode> {
  return execute(client.PATCH("/nodes/{nodeId}", { headers: requestHeaders("PATCH", token, revision, idempotencyKey), params: { path: { nodeId: id } }, body: node }));
}

export function deleteNode(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/nodes/{nodeId}", { headers: requestHeaders("DELETE", token, revision, idempotencyKey), params: { path: { nodeId: id } } }));
}

export function purgeNode(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.POST("/nodes/{nodeId}/actions/{action}", { headers: requestHeaders("POST", token, revision, idempotencyKey), params: { path: { nodeId: id, action: "purge" } } })).then(() => undefined);
}

export function bootstrapNode(id: string, input: NodeBootstrapRequest, token?: string, idempotencyKey?: string): Promise<NodeBootstrapResponse> {
  return execute(client.POST("/nodes/{nodeId}/bootstrap", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { nodeId: id } }, body: input }));
}

export function listNodeInstallations(token?: string): Promise<{ items: PendingNodeInstallation[] }> {
  return execute(client.GET("/node-installations", { headers: requestHeaders("GET", token) }));
}

export function createNodeInstallation(input: NodeInstallationRequest, token?: string, idempotencyKey?: string): Promise<NodeBootstrapResponse> {
  return execute(client.POST("/node-installations", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: input }));
}

export function reissueNodeInstallation(id: string, token?: string, idempotencyKey?: string): Promise<NodeBootstrapResponse> {
  return execute(client.POST("/node-installations/{nodeId}/reissue", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { nodeId: id } } }));
}

export function deleteNodeInstallation(id: string, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/node-installations/{nodeId}", { headers: requestHeaders("DELETE", token, undefined, idempotencyKey), params: { path: { nodeId: id } } }));
}

export function nodeAction(id: string, action: "drain" | "reconnect" | "resync" | "decommission" | "upgrade", token?: string, idempotencyKey?: string): Promise<Schemas["NodeActionResponse"]> {
  return execute(client.POST("/nodes/{nodeId}/actions/{action}", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { nodeId: id, action } } }));
}

export function getNodeUpdate(id: string, token?: string): Promise<NodeUpdateStatus> {
  return execute(client.GET("/nodes/{nodeId}/update", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
}

export function getControllerUpdate(token?: string): Promise<Schemas["ControllerUpdateResponse"]> {
  return execute(client.GET("/controller/update", { headers: requestHeaders("GET", token) }));
}

export function checkControllerUpdate(token?: string, idempotencyKey?: string): Promise<Schemas["ControllerUpdateResponse"]> {
  return execute(client.POST("/controller/update/check", { headers: requestHeaders("POST", token, undefined, idempotencyKey) }));
}

export function applyControllerUpdate(version?: string, token?: string, idempotencyKey?: string): Promise<Schemas["ControllerUpdateResponse"]> {
  return execute(client.POST("/controller/update/apply", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: version ? { version } : {} }));
}

export function getNodeSpec(id: string, token?: string): Promise<ControllerNodeSpec> {
  return execute(client.GET("/nodes/{nodeId}/spec", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
}

export function putNodeSpec(id: string, spec: ControllerNodeSpecInput, revision?: number, token?: string, idempotencyKey?: string): Promise<ControllerNodeSpec> {
  return execute(client.PUT("/nodes/{nodeId}/spec", { headers: requestHeaders("PUT", token, revision, idempotencyKey), params: { path: { nodeId: id } }, body: spec }));
}

export function deleteNodeSpec(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/nodes/{nodeId}/spec", { headers: requestHeaders("DELETE", token, revision, idempotencyKey), params: { path: { nodeId: id } } }));
}

export function getNodeEgress(id: string, token?: string): Promise<EgressPolicy> {
  return execute(client.GET("/nodes/{nodeId}/spec/egress", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
}

export function updateNodeEgress(id: string, policy: EgressPolicy, revision: number, token?: string, idempotencyKey?: string): Promise<EgressPolicy> {
  return execute(client.PUT("/nodes/{nodeId}/spec/egress", { headers: requestHeaders("PUT", token, revision, idempotencyKey), params: { path: { nodeId: id } }, body: policy }));
}

export function scheduleNode(id: string, token?: string, idempotencyKey?: string): Promise<Schemas["ScheduleNodeResponse"]> {
  return execute(client.POST("/nodes/{nodeId}/actions/schedule", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { nodeId: id } } }));
}

export function listServices(agentID?: string, token?: string): Promise<{ items: ControllerService[] }> {
  return execute(client.GET("/services", { headers: requestHeaders("GET", token), params: { query: agentID ? { agent_id: agentID } : {} } }));
}

export function createService(service: ControllerServiceInput, token?: string, idempotencyKey?: string): Promise<ControllerService> {
  return execute(client.POST("/services", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: service }));
}

export function updateService(id: string, service: Partial<ControllerServiceInput>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerService> {
  return execute(client.PATCH("/services/{serviceId}", { headers: requestHeaders("PATCH", token, revision, idempotencyKey), params: { path: { serviceId: id } }, body: service }));
}

export function deleteService(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/services/{serviceId}", { headers: requestHeaders("DELETE", token, revision, idempotencyKey), params: { path: { serviceId: id } } }));
}

export function listAssignments(token?: string): Promise<{ items: ControllerAssignment[] }> {
  return execute(client.GET("/assignments", { headers: requestHeaders("GET", token), params: { query: {} } }));
}

export function createAssignment(assignment: ControllerAssignmentInput, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> {
  return execute(client.POST("/assignments", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: assignment }));
}

export function getAssignment(id: string, token?: string): Promise<ControllerAssignment> {
  return execute(client.GET("/assignments/{assignmentId}", { headers: requestHeaders("GET", token), params: { path: { assignmentId: id } } }));
}

export function updateAssignment(id: string, assignment: Partial<ControllerAssignmentInput>, revision: number, token?: string, idempotencyKey?: string): Promise<ControllerAssignment> {
  return execute(client.PUT("/assignments/{assignmentId}", { headers: requestHeaders("PUT", token, revision, idempotencyKey), params: { path: { assignmentId: id } }, body: assignment as ControllerAssignmentInput }));
}

export function deleteAssignment(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/assignments/{assignmentId}", { headers: requestHeaders("DELETE", token, revision, idempotencyKey), params: { path: { assignmentId: id } } }));
}

export function listAudit(limit = 100, token?: string): Promise<{ items: ControllerAuditRecord[] }> {
  return execute(client.GET("/audit", { headers: requestHeaders("GET", token), params: { query: { limit } } }));
}

export function createEnrollmentToken(ttlSeconds?: number, token?: string, idempotencyKey?: string): Promise<Schemas["EnrollmentTokenResponse"]> {
  return execute(client.POST("/enrollment-tokens", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: { ttl_seconds: ttlSeconds } }));
}

export function listEnrollmentTokens(token?: string): Promise<{ items: EnrollmentTokenMeta[] }> {
  return execute(client.GET("/enrollment-tokens", { headers: requestHeaders("GET", token) }));
}

export function revokeEnrollmentToken(id: string, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/enrollment-tokens/{tokenId}", { headers: requestHeaders("DELETE", token, undefined, idempotencyKey), params: { path: { tokenId: id } } }));
}

export function listUserTokens(userID: string, token?: string): Promise<{ items: APITokenMeta[] }> {
  return execute(client.GET("/users/{userId}/tokens", { headers: requestHeaders("GET", token), params: { path: { userId: userID } } }));
}

export function createUserToken(userID: string, name: string, expiresAt?: string, token?: string, idempotencyKey?: string): Promise<Schemas["APITokenResponse"]> {
  return execute(client.POST("/users/{userId}/tokens", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { userId: userID } }, body: { name, expires_at: expiresAt ?? null } }));
}

export function revokeUserToken(userID: string, tokenID: string, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/users/{userId}/tokens/{tokenId}", { headers: requestHeaders("DELETE", token, undefined, idempotencyKey), params: { path: { userId: userID, tokenId: tokenID } } }));
}

export function listUsers(token?: string): Promise<{ items: ControllerUser[] }> {
  return execute(client.GET("/users", { headers: requestHeaders("GET", token) }));
}

export function createUser(input: Schemas["UserInput"], token?: string, idempotencyKey?: string): Promise<ControllerUser> {
  return execute(client.POST("/users", { headers: requestHeaders("POST", token, undefined, idempotencyKey), body: input }));
}

export function updateUser(id: string, input: Schemas["UserPatch"], revision: number, token?: string, idempotencyKey?: string): Promise<ControllerUser> {
  return execute(client.PATCH("/users/{userId}", { headers: requestHeaders("PATCH", token, revision, idempotencyKey), params: { path: { userId: id } }, body: input }));
}

export function deleteUser(id: string, revision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/users/{userId}", { headers: requestHeaders("DELETE", token, revision, idempotencyKey), params: { path: { userId: id } } }));
}

export function listEvents(limit = 100, token?: string): Promise<{ items: ControllerAuditRecord[] }> {
  return execute(client.GET("/events", { headers: requestHeaders("GET", token), params: { query: { limit } } }));
}

export function getObserved(id: string, token?: string): Promise<ControllerObservedState> {
  return execute(client.GET("/nodes/{nodeId}/observed", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
}

export function getSnapshot(id: string, token?: string): Promise<ControllerSnapshot> {
  return execute(client.GET("/nodes/{nodeId}/snapshot", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
}

type GlobalRuntimeQuery = NonNullable<paths["/runtime/connections"]["get"]["parameters"]["query"]>;

function parseGlobalRuntimeQuery(nodeID: string | undefined, query: string): GlobalRuntimeQuery {
  const params = new URLSearchParams(query);
  const result: GlobalRuntimeQuery = {};
  if (nodeID) result.node_id = nodeID;
  for (const key of ["state", "type", "source_ip", "peer_node_id", "gateway_id", "agent_id", "assignment_id", "service_id"] as const) {
    const value = params.get(key);
    if (value) result[key] = value;
  }
  const limit = params.get("limit");
  if (limit) result.limit = Number(limit);
  const protocol = params.get("protocol");
  if (protocol === "tcp" || protocol === "udp" || protocol === "quic") result.protocol = protocol;
  return result;
}

export function listRuntimeConnections(nodeID?: string, query = "", token?: string): Promise<{ items: RuntimeConnection[] }> {
  return execute(client.GET("/runtime/connections", { headers: requestHeaders("GET", token), params: { query: parseGlobalRuntimeQuery(nodeID, query) } }));
}

export function getNodeRuntimeConnections(nodeID: string, token?: string): Promise<{ items: RuntimeConnection[] }> {
  return execute(client.GET("/nodes/{nodeId}/runtime/connections", { headers: requestHeaders("GET", token), params: { path: { nodeId: nodeID }, query: { limit: 500 } } }));
}

export function getNodeRuntimeConnection(nodeID: string, connectionID: string, token?: string): Promise<RuntimeConnection> {
  return execute(client.GET("/nodes/{nodeId}/runtime/connections/{connectionId}", { headers: requestHeaders("GET", token), params: { path: { nodeId: nodeID, connectionId: connectionID } } }));
}

export function listRuntimeTraffic(nodeID?: string, token?: string): Promise<{ items: RuntimeTrafficRollup[] }> {
  return execute(client.GET("/runtime/traffic", { headers: requestHeaders("GET", token), params: { query: nodeID ? { node_id: nodeID } : {} } }));
}

export function listRuntimeEvents(nodeID?: string, token?: string, limit = 100): Promise<{ items: RuntimeEventRecord[] }> {
  return execute(client.GET("/runtime/events", { headers: requestHeaders("GET", token), params: { query: { node_id: nodeID, limit } } }));
}

export function getRuntimeSettings(token?: string): Promise<RuntimeSettings> {
  return execute(client.GET("/runtime/settings", { headers: requestHeaders("GET", token) }));
}

export function setRuntimeSettings(enabled: boolean, token?: string, idempotencyKey?: string): Promise<RuntimeSettings> {
  return execute(client.PUT("/runtime/settings", { headers: requestHeaders("PUT", token, undefined, idempotencyKey), body: { advanced_operations_enabled: enabled } }));
}

export function runtimeAction(nodeID: string, input: RuntimeActionInput, token?: string, idempotencyKey?: string): Promise<RuntimeActionResponse> {
  return execute(client.POST("/nodes/{nodeId}/runtime/actions", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { nodeId: nodeID } }, body: input }));
}

export function runtimeConnectionAction(nodeID: string, connectionID: string, input: Omit<RuntimeActionInput, "selector">, token?: string, idempotencyKey?: string): Promise<RuntimeActionResponse> {
  return execute(client.POST("/nodes/{nodeId}/runtime/connections/{connectionId}/actions", { headers: requestHeaders("POST", token, undefined, idempotencyKey), params: { path: { nodeId: nodeID, connectionId: connectionID } }, body: input }));
}

export function runtimeStreamURL(nodeID?: string): string {
  return `/api/v1/runtime/stream${nodeID ? `?node_id=${encodeURIComponent(nodeID)}` : ""}`;
}

export async function listNodeProxies(id: string, token?: string): Promise<{ items: ProxySpec[] }> {
  const result = await execute(client.GET("/nodes/{nodeId}/spec/proxies", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
  return { items: result.items ?? [] };
}

export function createNodeProxy(id: string, proxy: ProxySpec, specRevision: number, token?: string, idempotencyKey?: string): Promise<ProxySpec> {
  return execute(client.POST("/nodes/{nodeId}/spec/proxies", { headers: requestHeaders("POST", token, specRevision, idempotencyKey), params: { path: { nodeId: id } }, body: proxy }));
}

export function updateNodeProxy(id: string, proxyId: string, proxy: ProxySpec, specRevision: number, token?: string, idempotencyKey?: string): Promise<ProxySpec> {
  return execute(client.PUT("/nodes/{nodeId}/spec/proxies/{proxyId}", { headers: requestHeaders("PUT", token, specRevision, idempotencyKey), params: { path: { nodeId: id, proxyId } }, body: proxy }));
}

export function deleteNodeProxy(id: string, proxyId: string, specRevision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/nodes/{nodeId}/spec/proxies/{proxyId}", { headers: requestHeaders("DELETE", token, specRevision, idempotencyKey), params: { path: { nodeId: id, proxyId } } }));
}

export async function listNodeRoutes(id: string, token?: string): Promise<{ items: RouteRule[] }> {
  const result = await execute(client.GET("/nodes/{nodeId}/spec/routes", { headers: requestHeaders("GET", token), params: { path: { nodeId: id } } }));
  return { items: result.items ?? [] };
}

export function createNodeRoute(id: string, route: RouteRule, specRevision: number, token?: string, idempotencyKey?: string): Promise<RouteRule> {
  return execute(client.POST("/nodes/{nodeId}/spec/routes", { headers: requestHeaders("POST", token, specRevision, idempotencyKey), params: { path: { nodeId: id } }, body: route }));
}

export function updateNodeRoute(id: string, routeName: string, route: RouteRule, specRevision: number, token?: string, idempotencyKey?: string): Promise<RouteRule> {
  return execute(client.PUT("/nodes/{nodeId}/spec/routes/{routeName}", { headers: requestHeaders("PUT", token, specRevision, idempotencyKey), params: { path: { nodeId: id, routeName } }, body: route }));
}

export function deleteNodeRoute(id: string, routeName: string, specRevision: number, token?: string, idempotencyKey?: string): Promise<void> {
  return execute(client.DELETE("/nodes/{nodeId}/spec/routes/{routeName}", { headers: requestHeaders("DELETE", token, specRevision, idempotencyKey), params: { path: { nodeId: id, routeName } } }));
}
