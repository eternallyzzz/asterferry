<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import {
  ControllerAPIError,
  createAgent,
  createEnrollmentToken,
  createGateway,
  createNode,
  createService,
  createUser,
  deleteNode,
  deleteService,
  deleteUser,
  listAgents,
  listAssignments,
  listAudit,
  listGateways,
  listEnrollmentTokens,
  listNodes,
  listServices,
  listUsers,
  nodeAction,
  scheduleAgent,
  updateAgent,
  updateGateway,
  updateNode,
  updateService,
  updateUser,
  type ControllerAssignment,
  type ControllerAuditRecord,
  type ControllerNode,
  type ControllerService,
  type ControllerUser,
  type CreateNodeInput,
} from "../controller-api";

type Tab = "overview" | "nodes" | "services" | "assignments" | "audit" | "admin";
type SpecDocument = Record<string, unknown>;
type SpecItem = { node: ControllerNode; spec?: SpecDocument };

const props = defineProps<{ user: ControllerUser }>();
const emit = defineEmits<{ logout: [] }>();

const tabs: Array<{ id: Tab; label: string }> = [
  { id: "overview", label: "概览" },
  { id: "nodes", label: "节点与规格" },
  { id: "services", label: "服务" },
  { id: "assignments", label: "调度" },
  { id: "audit", label: "审计" },
];

const tab = ref<Tab>("overview");
const loading = ref(false);
const error = ref("");
const notice = ref("");
const nodes = ref<ControllerNode[]>([]);
const services = ref<ControllerService[]>([]);
const assignments = ref<ControllerAssignment[]>([]);
const audit = ref<ControllerAuditRecord[]>([]);
const gateways = ref<SpecItem[]>([]);
const agents = ref<SpecItem[]>([]);
const users = ref<ControllerUser[]>([]);
const enrollmentTokens = ref<unknown[]>([]);
let timer: number | undefined;

const canOperate = computed(() => props.user.role === "operator" || props.user.role === "admin");
const canAdmin = computed(() => props.user.role === "admin");
const gatewayNodes = computed(() => nodes.value.filter((node) => node.role === "gateway"));
const agentNodes = computed(() => nodes.value.filter((node) => node.role === "agent"));
const onlineNodes = computed(() => nodes.value.filter((node) => node.enabled && node.certificate_state === "active"));
const activeAssignments = computed(() => assignments.value.filter((item) => item.state === "applied"));

const nodeForm = ref({ id: "", role: "gateway" as ControllerNode["role"], name: "", labels: "{}", enabled: true });
const nodeEditID = ref("");
const nodeRevision = ref(0);
const serviceForm = ref({ id: "", agent_id: "", protocol: "tcp" as ControllerService["protocol"], local_target: "127.0.0.1:8080", public_bind: "0.0.0.0", public_port: "0", selector: "{}", enabled: true });
const serviceEditID = ref("");
const serviceRevision = ref(0);

const selectedGatewayID = ref("");
const selectedAgentID = ref("");
const gatewaySpecText = ref("");
const agentSpecText = ref("");
const gatewaySpecRevision = ref<number | undefined>();
const agentSpecRevision = ref<number | undefined>();

const userForm = ref({ username: "", password: "", role: "viewer" as ControllerUser["role"] });
const enrollmentRole = ref<"gateway" | "agent">("gateway");
const enrollmentTTL = ref("900");
const issuedToken = ref("");

function key(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function describeError(caught: unknown): string {
  if (caught instanceof ControllerAPIError) {
    if (caught.code === "revision_conflict") return "资源已被其他操作者修改，请刷新后重试（revision 冲突）。";
    if (caught.code === "already_exists") return "资源已存在，请使用其他 ID。";
    if (caught.status === 409) return caught.message || "请求冲突，请刷新后重试。";
    if (caught.status === 403) return "当前账户没有执行此操作的权限。";
    return caught.message;
  }
  return caught instanceof Error ? caught.message : "Controller 请求失败。";
}

function setFailure(caught: unknown) {
  error.value = describeError(caught);
  notice.value = "";
}

function parseObject(value: string, field: string): Record<string, unknown> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value || "{}");
  } catch {
    throw new Error(`${field} 必须是有效 JSON。`);
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(`${field} 必须是 JSON 对象。`);
  return parsed as Record<string, unknown>;
}

function parseLabels(value: string): Record<string, string> {
  const parsed = parseObject(value, "labels");
  const labels: Record<string, string> = {};
  for (const [name, item] of Object.entries(parsed)) {
    if (typeof item !== "string") throw new Error("labels 的值必须是字符串。");
    labels[name] = item;
  }
  return labels;
}

function pretty(value: unknown): string {
  try { return JSON.stringify(value, null, 2); } catch { return "—"; }
}

function defaultGatewaySpec(nodeID: string): SpecDocument {
  return {
    node_id: nodeID,
    public_endpoints: ["127.0.0.1:4433"],
    listeners: [],
    labels: {},
    capacity: { max_agents: 128, max_connections: 4096, max_services: 4096 },
    port_pool: { tcp: [{ min: 20000, max: 20100 }], udp: [{ min: 21000, max: 21100 }] },
    transport: { alpn: "asterferry-data/1", max_streams: 1024, max_frame_bytes: 65536, max_datagram_bytes: 65536, handshake_timeout_seconds: 10, idle_timeout_seconds: 300 },
    obfuscation: { mode: "standard", max_padding_bytes: 0, handshake_shaping: false },
    egress: { enabled: false, max_connections: 0 },
  };
}

function defaultAgentSpec(nodeID: string): SpecDocument {
  return {
    node_id: nodeID,
    gateway_selector: { match_labels: {} },
    proxies: [],
    routes: [],
    limits: { max_connections: 4096, max_streams: 1024, max_buffer_bytes: 67108864 },
    egress: { enabled: false, max_connections: 0 },
    logging: { level: "info", format: "json" },
  };
}

function selectGateway(id: string) {
  selectedGatewayID.value = id;
  const item = gateways.value.find((entry) => entry.node.id === id);
  gatewaySpecRevision.value = typeof item?.spec?.revision === "number" ? item.spec.revision : undefined;
  gatewaySpecText.value = pretty(item?.spec || defaultGatewaySpec(id));
}

function selectAgent(id: string) {
  selectedAgentID.value = id;
  const item = agents.value.find((entry) => entry.node.id === id);
  agentSpecRevision.value = typeof item?.spec?.revision === "number" ? item.spec.revision : undefined;
  agentSpecText.value = pretty(item?.spec || defaultAgentSpec(id));
}

async function refresh() {
  loading.value = true;
  try {
    const [nodeResult, serviceResult, assignmentResult, auditResult, gatewayResult, agentResult] = await Promise.all([
      listNodes(), listServices(), listAssignments(), listAudit(100), listGateways(), listAgents(),
    ]);
    nodes.value = nodeResult.items;
    services.value = serviceResult.items;
    assignments.value = assignmentResult.items;
    audit.value = auditResult.items;
    gateways.value = gatewayResult.items.map((item) => ({ node: item.node, spec: item.spec && typeof item.spec === "object" ? item.spec as SpecDocument : undefined }));
    agents.value = agentResult.items.map((item) => ({ node: item.node, spec: item.spec && typeof item.spec === "object" ? item.spec as SpecDocument : undefined }));
    if (!selectedGatewayID.value && gateways.value[0]) selectGateway(gateways.value[0].node.id);
    if (!selectedAgentID.value && agents.value[0]) selectAgent(agents.value[0].node.id);
    if (canAdmin.value) {
      const [userResult, tokenResult] = await Promise.all([listUsers(), listEnrollmentTokens()]);
      users.value = userResult.items;
      enrollmentTokens.value = tokenResult.items;
    }
    error.value = "";
  } catch (caught) {
    setFailure(caught);
  } finally {
    loading.value = false;
  }
}

function tokenField(item: unknown, field: "id" | "role" | "expires_at" | "used_at"): string {
  if (!item || typeof item !== "object") return "";
  const value = (item as Record<string, unknown>)[field];
  return value == null ? "" : String(value);
}

async function copyIssuedToken() {
  if (!issuedToken.value || !navigator.clipboard) return;
  try {
    await navigator.clipboard.writeText(issuedToken.value);
    notice.value = "token 已复制到剪贴板。";
  } catch {
    notice.value = "浏览器拒绝访问剪贴板，请手动复制 token。";
  }
}

function resetNode() {
  nodeForm.value = { id: "", role: "gateway", name: "", labels: "{}", enabled: true };
  nodeEditID.value = "";
  nodeRevision.value = 0;
}

function editNode(node: ControllerNode) {
  nodeForm.value = { id: node.id, role: node.role, name: node.name, labels: pretty(node.labels || {}), enabled: node.enabled };
  nodeEditID.value = node.id;
  nodeRevision.value = node.revision;
  tab.value = "nodes";
}

async function saveNode() {
  if (!canAdmin.value) return;
  try {
    const input: CreateNodeInput = { id: nodeForm.value.id.trim(), role: nodeForm.value.role, name: nodeForm.value.name.trim(), labels: parseLabels(nodeForm.value.labels), enabled: nodeForm.value.enabled };
    if (nodeEditID.value) {
      await updateNode(nodeEditID.value, { role: input.role, name: input.name, labels: input.labels, enabled: input.enabled }, nodeRevision.value, undefined, key());
      notice.value = `节点 ${nodeEditID.value} 已更新。`;
    } else {
      await createNode(input, undefined, key());
      notice.value = `节点 ${input.id} 已创建。`;
    }
    resetNode();
    await refresh();
  } catch (caught) { setFailure(caught); }
}

async function removeNode(node: ControllerNode) {
  if (!canAdmin.value || !window.confirm(`删除节点 ${node.id}？`)) return;
  try { await deleteNode(node.id, node.revision, undefined, key()); notice.value = `节点 ${node.id} 已删除。`; await refresh(); } catch (caught) { setFailure(caught); }
}

async function runNodeAction(node: ControllerNode, action: "drain" | "reconnect" | "resync") {
  if (!canOperate.value) return;
  try { const result = await nodeAction(node.id, action, undefined, key()); notice.value = `${node.id}：${action} ${result.state}。`; await refresh(); } catch (caught) { setFailure(caught); }
}

function resetService() {
  serviceForm.value = { id: "", agent_id: agentNodes.value[0]?.id || "", protocol: "tcp", local_target: "127.0.0.1:8080", public_bind: "0.0.0.0", public_port: "0", selector: "{}", enabled: true };
  serviceEditID.value = "";
  serviceRevision.value = 0;
}

function editService(service: ControllerService) {
  serviceForm.value = { id: service.id, agent_id: service.agent_id, protocol: service.protocol, local_target: service.local_target, public_bind: service.public_bind, public_port: String(service.public_port), selector: pretty(service.gateway_selector || {}), enabled: service.enabled };
  serviceEditID.value = service.id;
  serviceRevision.value = service.revision;
  tab.value = "services";
}

async function saveService() {
  if (!canOperate.value) return;
  try {
    const selector = parseObject(serviceForm.value.selector, "gateway_selector");
    const service = { id: serviceForm.value.id.trim(), agent_id: serviceForm.value.agent_id.trim(), protocol: serviceForm.value.protocol, local_target: serviceForm.value.local_target.trim(), public_bind: serviceForm.value.public_bind.trim(), public_port: Number(serviceForm.value.public_port) || 0, gateway_selector: selector as { match_labels?: Record<string, string> }, enabled: serviceForm.value.enabled };
    if (serviceEditID.value) {
      const { id: _id, ...patch } = service;
      await updateService(serviceEditID.value, patch, serviceRevision.value, undefined, key());
      notice.value = `服务 ${serviceEditID.value} 已更新。`;
    } else {
      await createService(service, undefined, key());
      notice.value = `服务 ${service.id} 已创建。`;
    }
    resetService();
    await refresh();
  } catch (caught) { setFailure(caught); }
}

async function removeService(service: ControllerService) {
  if (!canOperate.value || !window.confirm(`删除服务 ${service.id}？`)) return;
  try { await deleteService(service.id, service.revision, undefined, key()); notice.value = `服务 ${service.id} 已删除。`; await refresh(); } catch (caught) { setFailure(caught); }
}

async function saveSpec(kind: "gateway" | "agent") {
  if (!canOperate.value) return;
  const id = kind === "gateway" ? selectedGatewayID.value : selectedAgentID.value;
  if (!id) return;
  try {
    const document = parseObject(kind === "gateway" ? gatewaySpecText.value : agentSpecText.value, `${kind} spec`);
    document.node_id = id;
    const revision = kind === "gateway" ? gatewaySpecRevision.value : agentSpecRevision.value;
    const result = kind === "gateway"
      ? (revision === undefined ? await createGateway(document, undefined, key()) : await updateGateway(id, document, revision, undefined, key()))
      : (revision === undefined ? await createAgent(document, undefined, key()) : await updateAgent(id, document, revision, undefined, key()));
    const returned = result && typeof result === "object" ? result as { revision?: unknown } : undefined;
    if (kind === "gateway") gatewaySpecRevision.value = typeof returned?.revision === "number" ? returned.revision : undefined;
    else agentSpecRevision.value = typeof returned?.revision === "number" ? returned.revision : undefined;
    notice.value = `${kind === "gateway" ? "Gateway" : "Agent"} 规格 ${id} 已保存。`;
    await refresh();
  } catch (caught) { setFailure(caught); }
}

async function schedule(agent: ControllerNode) {
  if (!canOperate.value) return;
  try { await scheduleAgent(agent.id, undefined, key()); notice.value = `已请求为 ${agent.id} 重新调度。`; await refresh(); } catch (caught) { setFailure(caught); }
}

async function createControllerUser() {
  if (!canAdmin.value) return;
  try {
    await createUser(userForm.value, undefined, key());
    notice.value = `用户 ${userForm.value.username} 已创建。`;
    userForm.value = { username: "", password: "", role: "viewer" };
    await refresh();
  } catch (caught) { setFailure(caught); }
}

async function toggleUser(user: ControllerUser) {
  if (!canAdmin.value) return;
  try { await updateUser(user.id, { enabled: !user.enabled }, user.revision, undefined, key()); notice.value = `用户 ${user.username} 已${user.enabled ? "禁用" : "启用"}。`; await refresh(); } catch (caught) { setFailure(caught); }
}

async function removeUser(user: ControllerUser) {
  if (!canAdmin.value || !window.confirm(`删除用户 ${user.username}？`)) return;
  try { await deleteUser(user.id, user.revision, undefined, key()); notice.value = `用户 ${user.username} 已删除。`; await refresh(); } catch (caught) { setFailure(caught); }
}

async function issueEnrollmentToken() {
  if (!canAdmin.value) return;
  try {
    const result = await createEnrollmentToken(enrollmentRole.value, Math.max(60, Math.min(900, Number(enrollmentTTL.value) || 900)), undefined, key());
    issuedToken.value = result.token;
    notice.value = "令牌已创建；它只显示在当前页面，请立即复制。";
    await refresh();
  } catch (caught) { setFailure(caught); }
}

function roleLabel(role: string): string { return role === "gateway" ? "Gateway" : role === "agent" ? "Agent" : role; }
function stateLabel(state: string): string { return ({ applied: "已应用", pending: "等待", degraded: "降级", draining: "排空" } as Record<string, string>)[state] || state || "未知"; }
function certificateTone(state: string): string { return state === "active" ? "good" : state === "revoked" || state === "expired" ? "bad" : "warn"; }

onMounted(() => { void refresh(); timer = window.setInterval(() => void refresh(), 10000); });
onUnmounted(() => { if (timer !== undefined) window.clearInterval(timer); });
</script>

<template>
  <main class="app-shell controller-shell">
    <header class="app-header">
      <div>
        <div class="section-kicker">ASTERFERRY CONTROLLER · AFDP/1</div>
        <h1>控制面</h1>
        <p class="muted">{{ props.user.username }} · {{ props.user.role }} · Cookie 会话与 CSRF 保护</p>
      </div>
      <div class="header-actions">
        <button class="secondary-button" :disabled="loading" @click="refresh">{{ loading ? "刷新中…" : "刷新" }}</button>
        <button class="secondary-button" @click="emit('logout')">退出</button>
      </div>
    </header>

    <div class="controller-content">
      <nav class="controller-tabs" aria-label="Controller sections">
        <button v-for="item in tabs" :key="item.id" :class="['tab-button', { active: tab === item.id }]" @click="tab = item.id">{{ item.label }}</button>
        <button v-if="canAdmin" :class="['tab-button', { active: tab === 'admin' }]" @click="tab = 'admin'">管理</button>
      </nav>
      <p v-if="error" class="global-error">{{ error }} <button class="link-button" @click="error = ''">关闭</button></p>
      <p v-if="notice" class="global-notice">{{ notice }}</p>

      <section v-if="tab === 'overview'" class="page-stack">
        <div class="page-heading"><div><p class="eyebrow">CONTROL PLANE</p><h2>运行概览</h2><p class="muted">所有配置写入 Controller，节点只应用带 generation 和 checksum 的快照。</p></div></div>
        <div class="metric-grid controller-metrics">
          <article class="metric-card"><span>节点</span><strong>{{ nodes.length }}</strong></article>
          <article class="metric-card"><span>在线节点</span><strong class="good-text">{{ onlineNodes.length }}</strong></article>
          <article class="metric-card"><span>Gateway / Agent</span><strong>{{ gatewayNodes.length }} / {{ agentNodes.length }}</strong></article>
          <article class="metric-card"><span>服务</span><strong>{{ services.length }}</strong></article>
          <article class="metric-card"><span>已应用调度</span><strong>{{ activeAssignments.length }}</strong></article>
          <article class="metric-card"><span>审计记录</span><strong>{{ audit.length }}</strong></article>
        </div>
        <div class="panel-grid two-columns">
          <article class="panel"><div class="panel-title"><h3>最近节点</h3><button class="link-button" @click="tab = 'nodes'">管理节点 →</button></div><div v-if="nodes.length" class="compact-list"><div v-for="node in nodes.slice(0, 6)" :key="node.id" class="compact-row"><span><strong>{{ node.name }}</strong><small>{{ roleLabel(node.role) }} · <code>{{ node.id }}</code></small></span><span :class="['status-pill', certificateTone(node.certificate_state)]">{{ node.certificate_state }}</span></div></div><p v-else class="empty">尚未注册节点。</p></article>
          <article class="panel"><div class="panel-title"><h3>最近审计</h3><button class="link-button" @click="tab = 'audit'">查看全部 →</button></div><div v-if="audit.length" class="compact-list"><div v-for="item in audit.slice(0, 6)" :key="item.id" class="compact-row"><span><strong>{{ item.action }} · {{ item.resource }}</strong><small>{{ item.actor }} · {{ item.resource_id }}</small></span><time>{{ new Date(item.created_at).toLocaleString('zh-CN', { hour12: false }) }}</time></div></div><p v-else class="empty">暂无审计记录。</p></article>
        </div>
      </section>

      <section v-if="tab === 'nodes'" class="page-stack">
        <div class="page-heading"><div><p class="eyebrow">NODES AND SPECS</p><h2>节点与规格</h2><p class="muted">身份由 Admin 管理，Gateway/Agent 业务规格由 Operator 管理。</p></div></div>
        <div class="panel-grid two-columns">
          <article class="panel"><div class="panel-title"><h3>节点清单</h3><span class="muted">{{ nodes.length }} 个</span></div><div class="data-table-wrap"><table class="data-table"><thead><tr><th>节点</th><th>角色</th><th>证书</th><th>状态</th><th>Revision</th><th>操作</th></tr></thead><tbody><tr v-for="node in nodes" :key="node.id"><td><strong>{{ node.name }}</strong><small><code>{{ node.id }}</code></small></td><td>{{ roleLabel(node.role) }}</td><td><span :class="['status-pill', certificateTone(node.certificate_state)]">{{ node.certificate_state }}</span></td><td>{{ node.enabled ? '启用' : '停用' }}</td><td>{{ node.revision }}</td><td><div class="row-actions"><button class="link-button" :disabled="!canAdmin" @click="editNode(node)">编辑</button><button class="link-button" :disabled="!canOperate" @click="runNodeAction(node, 'resync')">对账</button><button class="link-button" :disabled="!canOperate" @click="runNodeAction(node, 'drain')">排空</button><button class="danger-link" :disabled="!canAdmin" @click="removeNode(node)">删除</button></div></td></tr></tbody></table></div><p v-if="!nodes.length" class="empty">暂无节点。使用右侧表单注册第一个 Gateway 或 Agent。</p></article>
          <article class="panel form-panel"><div class="panel-title"><h3>{{ nodeEditID ? '编辑节点' : '注册节点' }}</h3><button v-if="nodeEditID" class="link-button" @click="resetNode">新建</button></div><form @submit.prevent="saveNode"><label>Node ID<input v-model="nodeForm.id" :disabled="Boolean(nodeEditID)" required placeholder="gw-east" /></label><label>角色<select v-model="nodeForm.role" :disabled="Boolean(nodeEditID)"><option value="gateway">Gateway</option><option value="agent">Agent</option></select></label><label>名称<input v-model="nodeForm.name" required placeholder="East Gateway" /></label><label>标签 JSON<textarea v-model="nodeForm.labels" rows="3" spellcheck="false" placeholder='{"region":"east"}' /></label><label class="check-label"><input v-model="nodeForm.enabled" type="checkbox" /> 启用节点</label><button class="primary-button" type="submit" :disabled="!canAdmin">{{ nodeEditID ? '保存节点' : '注册节点' }}</button></form><p v-if="nodeEditID" class="muted-note">If-Match revision: {{ nodeRevision }}。并发修改会被 Controller 拒绝。</p><p v-else class="muted-note">注册后在“管理”中创建 enrollment token，再运行节点 enroll。</p></article>
        </div>
        <div class="panel-grid two-columns"><article class="panel form-panel"><div class="panel-title"><h3>GatewaySpec</h3><select v-model="selectedGatewayID" @change="selectGateway(selectedGatewayID)"><option value="" disabled>选择 Gateway</option><option v-for="item in gateways" :key="item.node.id" :value="item.node.id">{{ item.node.name }} ({{ item.node.id }})</option></select></div><textarea v-model="gatewaySpecText" class="json-editor" spellcheck="false" /><div class="form-footer"><span class="muted">revision {{ gatewaySpecRevision ?? 'new' }}</span><button class="primary-button" :disabled="!canOperate || !selectedGatewayID" @click="saveSpec('gateway')">保存 Gateway 规格</button></div></article><article class="panel form-panel"><div class="panel-title"><h3>AgentSpec</h3><select v-model="selectedAgentID" @change="selectAgent(selectedAgentID)"><option value="" disabled>选择 Agent</option><option v-for="item in agents" :key="item.node.id" :value="item.node.id">{{ item.node.name }} ({{ item.node.id }})</option></select></div><textarea v-model="agentSpecText" class="json-editor" spellcheck="false" /><div class="form-footer"><span class="muted">revision {{ agentSpecRevision ?? 'new' }}</span><button class="primary-button" :disabled="!canOperate || !selectedAgentID" @click="saveSpec('agent')">保存 Agent 规格</button></div></article></div>
      </section>

      <section v-if="tab === 'services'" class="page-stack"><div class="page-heading"><div><p class="eyebrow">SERVICE CATALOG</p><h2>服务</h2><p class="muted">创建 TCP/UDP reverse 服务；public_port 为 0 时由 Gateway 端口池自动分配。</p></div></div><div class="panel-grid two-columns"><article class="panel"><div class="panel-title"><h3>服务清单</h3><span class="muted">{{ services.length }} 个</span></div><div class="data-table-wrap"><table class="data-table"><thead><tr><th>服务</th><th>Agent</th><th>协议</th><th>绑定</th><th>状态</th><th>操作</th></tr></thead><tbody><tr v-for="service in services" :key="service.id"><td><strong>{{ service.id }}</strong><small>{{ service.local_target }}</small></td><td><code>{{ service.agent_id }}</code></td><td>{{ service.protocol.toUpperCase() }}</td><td>{{ service.public_bind }}:{{ service.public_port || '自动' }}</td><td>{{ service.enabled ? '启用' : '停用' }}</td><td><div class="row-actions"><button class="link-button" :disabled="!canOperate" @click="editService(service)">编辑</button><button class="danger-link" :disabled="!canOperate" @click="removeService(service)">删除</button></div></td></tr></tbody></table></div><p v-if="!services.length" class="empty">暂无服务。</p></article><article class="panel form-panel"><div class="panel-title"><h3>{{ serviceEditID ? '编辑服务' : '创建服务' }}</h3><button v-if="serviceEditID" class="link-button" @click="resetService">新建</button></div><form @submit.prevent="saveService"><label>Service ID<input v-model="serviceForm.id" :disabled="Boolean(serviceEditID)" required placeholder="web-tcp" /></label><label>Agent<select v-model="serviceForm.agent_id" required><option v-for="agent in agentNodes" :key="agent.id" :value="agent.id">{{ agent.name }} ({{ agent.id }})</option></select></label><div class="form-row"><label>协议<select v-model="serviceForm.protocol"><option value="tcp">TCP</option><option value="udp">UDP</option></select></label><label>公网端口<input v-model="serviceForm.public_port" type="number" min="0" max="65535" /></label></div><label>本地目标<input v-model="serviceForm.local_target" required placeholder="10.0.0.5:8080" /></label><label>公网绑定<input v-model="serviceForm.public_bind" required placeholder="0.0.0.0" /></label><label>Gateway selector JSON<textarea v-model="serviceForm.selector" rows="3" spellcheck="false" placeholder='{"match_labels":{"region":"east"}}' /></label><label class="check-label"><input v-model="serviceForm.enabled" type="checkbox" /> 启用服务</label><button class="primary-button" type="submit" :disabled="!canOperate || !agentNodes.length">{{ serviceEditID ? '保存服务' : '创建服务' }}</button></form></article></div></section>

      <section v-if="tab === 'assignments'" class="page-stack"><div class="page-heading"><div><p class="eyebrow">SCHEDULER</p><h2>调度与 Assignment</h2><p class="muted">Controller 根据 selector、容量和端口占用保持健康 assignment；Agent 重连后自动对账。</p></div></div><article class="panel"><div class="data-table-wrap"><table class="data-table"><thead><tr><th>Assignment</th><th>Gateway</th><th>Agent</th><th>服务</th><th>Generation</th><th>状态</th><th>端点</th></tr></thead><tbody><tr v-for="item in assignments" :key="item.id"><td><strong>{{ item.id }}</strong><small>revision {{ item.revision ?? '—' }}</small></td><td><code>{{ item.gateway_id }}</code></td><td><code>{{ item.agent_id }}</code></td><td>{{ item.service_ids.length }}</td><td>{{ item.generation }}</td><td><span :class="['status-pill', item.state === 'applied' ? 'good' : 'warn']">{{ stateLabel(item.state) }}</span></td><td>{{ item.public_endpoint || '—' }}</td></tr></tbody></table></div><p v-if="!assignments.length" class="empty">暂无 assignment。创建服务并点击 Agent 的“重新调度”。</p></article><article class="panel"><div class="panel-title"><h3>Agent 调度</h3><span class="muted">普通运行时动作需要 Operator</span></div><div class="compact-list"><div v-for="agent in agentNodes" :key="agent.id" class="compact-row"><span><strong>{{ agent.name }}</strong><small><code>{{ agent.id }}</code></small></span><button class="secondary-button" :disabled="!canOperate" @click="schedule(agent)">重新调度</button></div></div></article></section>

      <section v-if="tab === 'audit'" class="page-stack"><div class="page-heading"><div><p class="eyebrow">AUDIT LOG</p><h2>审计</h2><p class="muted">资源写入、运行时动作和节点事件统一记录在 Controller SQLite。</p></div></div><article class="panel"><div class="data-table-wrap"><table class="data-table"><thead><tr><th>时间</th><th>Actor</th><th>Action</th><th>Resource</th><th>ID</th><th>Revision</th><th>Attributes</th></tr></thead><tbody><tr v-for="item in audit" :key="item.id"><td>{{ new Date(item.created_at).toLocaleString('zh-CN', { hour12: false }) }}</td><td>{{ item.actor }}</td><td>{{ item.action }}</td><td>{{ item.resource }}</td><td><code>{{ item.resource_id || '—' }}</code></td><td>{{ item.revision || '—' }}</td><td><code>{{ pretty(item.attributes || {}) }}</code></td></tr></tbody></table></div><p v-if="!audit.length" class="empty">暂无审计记录。</p></article></section>

      <section v-if="tab === 'admin' && canAdmin" class="page-stack"><div class="page-heading"><div><p class="eyebrow">IDENTITY AND ENROLLMENT</p><h2>管理</h2><p class="muted">用户、API token 和节点 enrollment token 只由 Admin 管理。</p></div></div><div class="panel-grid two-columns"><article class="panel form-panel"><div class="panel-title"><h3>创建 Controller 用户</h3></div><form @submit.prevent="createControllerUser"><label>用户名<input v-model="userForm.username" required autocomplete="off" /></label><label>初始密码<input v-model="userForm.password" required type="password" autocomplete="new-password" /></label><label>角色<select v-model="userForm.role"><option value="viewer">Viewer</option><option value="operator">Operator</option><option value="admin">Admin</option></select></label><button class="primary-button" type="submit">创建用户</button></form></article><article class="panel form-panel"><div class="panel-title"><h3>创建 enrollment token</h3></div><form @submit.prevent="issueEnrollmentToken"><div class="form-row"><label>节点角色<select v-model="enrollmentRole"><option value="gateway">Gateway</option><option value="agent">Agent</option></select></label><label>有效期（秒）<input v-model="enrollmentTTL" type="number" min="60" max="900" /></label></div><button class="primary-button" type="submit">生成一次性 token</button></form><div v-if="issuedToken" class="secret-output"><span>仅显示一次</span><code>{{ issuedToken }}</code><button class="link-button" @click="copyIssuedToken">复制</button></div></article></div><article class="panel"><div class="panel-title"><h3>用户</h3><span class="muted">{{ users.length }} 个</span></div><div class="data-table-wrap"><table class="data-table"><thead><tr><th>用户名</th><th>角色</th><th>状态</th><th>Revision</th><th>操作</th></tr></thead><tbody><tr v-for="user in users" :key="user.id"><td><strong>{{ user.username }}</strong></td><td>{{ user.role }}</td><td>{{ user.enabled ? '启用' : '停用' }}</td><td>{{ user.revision }}</td><td><div class="row-actions"><button class="link-button" @click="toggleUser(user)">{{ user.enabled ? '禁用' : '启用' }}</button><button class="danger-link" @click="removeUser(user)">删除</button></div></td></tr></tbody></table></div></article><article class="panel"><div class="panel-title"><h3>Enrollment token 历史</h3><span class="muted">哈希不会返回</span></div><div class="data-table-wrap"><table class="data-table"><thead><tr><th>ID</th><th>角色</th><th>过期时间</th><th>使用时间</th></tr></thead><tbody><tr v-for="item in enrollmentTokens" :key="tokenField(item, 'id')"><td><code>{{ tokenField(item, 'id') || '—' }}</code></td><td>{{ tokenField(item, 'role') || '—' }}</td><td>{{ tokenField(item, 'expires_at') || '—' }}</td><td>{{ tokenField(item, 'used_at') || '未使用' }}</td></tr></tbody></table></div><p v-if="!enrollmentTokens.length" class="empty">暂无 token 记录。</p></article></section>
    </div>
  </main>
</template>
