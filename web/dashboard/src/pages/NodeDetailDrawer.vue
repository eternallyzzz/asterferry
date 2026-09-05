<script setup lang="ts">
import { computed, ref, watch } from "vue";
import DrawerPanel from "../components/ui/DrawerPanel.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import JsonEditor from "../components/ui/JsonEditor.vue";
import ModalDialog from "../components/ui/ModalDialog.vue";
import Spinner from "../components/ui/Spinner.vue";
import EgressForm from "../components/nodes/EgressForm.vue";
import ProxyRouteManager from "../components/nodes/ProxyRouteManager.vue";
import ObservedView from "../components/nodes/ObservedView.vue";
import SnapshotView from "../components/nodes/SnapshotView.vue";
import {
	bootstrapNode,
	ControllerAPIError,
  deleteNodeSpec,
	getNodeSpec,
	listNodes,
	nodeAction,
	putNodeSpec,
	type ControllerAgentSpec,
	type ControllerAgentSpecInput,
	type ControllerGatewaySpecInput,
	type ControllerNode,
	type ControllerNodeSpecInput,
	type EgressPolicy,
	type NodeBootstrapResponse,
  type NodeSpecKind,
  type ProxySpec,
  type RouteRule,
} from "../controller-api";
import { useNotify } from "../composables/useNotify";
import { useSession } from "../session";
import { certificateTone, describeError, formatTime, newIdempotencyKey, parseObject, prettyJson } from "../utils/format";

// The editor intentionally keeps the decoded JSON document intact. The type
// assertion at save time is the boundary where arbitrary editor JSON enters
// the strongly typed API model, so protected obfuscation fields survive a
// read/edit/write round trip without being copied into operational summaries.
type SpecDocument = ControllerGatewaySpecInput | ControllerAgentSpec;
type Section = "info" | "spec" | "egress" | "proxies" | "routes" | "observed" | "snapshot";

const props = defineProps<{ open: boolean; node: ControllerNode | null }>();
const emit = defineEmits<{ close: []; changed: [] }>();

const notify = useNotify();
const session = useSession();

const section = ref<Section>("info");
const visited = ref<Set<Section>>(new Set(["info"]));
const specDoc = ref<SpecDocument | undefined>();
const specText = ref("");
const specRevision = ref<number | undefined>();
const specKind = ref<NodeSpecKind | undefined>();
const selectedKind = ref<NodeSpecKind>("agent");
const selectedGatewayID = ref("");
const gatewayNodes = ref<ControllerNode[]>([]);
const gatewayLoading = ref(false);
const gatewayError = ref("");
const specValid = ref(true);
const specLoading = ref(false);
const specSaving = ref(false);
const specError = ref("");
const confirmDeleteSpec = ref(false);
const deletingSpec = ref(false);
const installOpen = ref(false);
const installPlatform = ref<"linux" | "windows">("linux");
const installArch = ref<"amd64" | "arm64">("amd64");
const installResult = ref<NodeBootstrapResponse | null>(null);
const installError = ref("");
const installing = ref(false);
const copiedInstallCommand = ref(false);
let gatewayRequestVersion = 0;

const activeKind = computed(() => specKind.value ?? selectedKind.value);
const nodeEnrolled = computed(() => props.node?.certificate_state === "active" && Boolean(props.node.certificate_serial));
const specSaveDisabled = computed(() => (
  !session.canOperate.value ||
  !nodeEnrolled.value ||
  !specValid.value ||
  specSaving.value ||
  (selectedKind.value === "agent" && !selectedGatewayID.value)
));

function specKindLabel(kind?: NodeSpecKind): string {
  return kind === "gateway" ? "Gateway" : kind === "agent" ? "Agent" : "未配置";
}

const sections = computed(() => {
  const base: Array<{ id: Section; label: string }> = [
    { id: "info", label: "信息" },
    { id: "spec", label: "规格" },
    { id: "egress", label: "出口策略" },
  ];
  // An unconfigured Node has no behavior-specific panels yet. The draft kind
  // is only a form choice; it must not make the identity look like an Agent
  // before the spec is actually persisted.
  if (specKind.value === "agent") {
    base.push({ id: "proxies", label: "代理" }, { id: "routes", label: "路由" });
  }
  base.push({ id: "observed", label: "观测" }, { id: "snapshot", label: "快照" });
  return base;
});

const specEgress = computed(() => specDoc.value?.egress as EgressPolicy | undefined);
const specProxies = computed(() => {
  const document = specDoc.value;
  return document && "proxies" in document && Array.isArray(document.proxies) ? document.proxies as ProxySpec[] : [];
});
const specRoutes = computed(() => {
  const document = specDoc.value;
  return document && "routes" in document && Array.isArray(document.routes) ? document.routes as RouteRule[] : [];
});

function defaultSpec(node: ControllerNode, kind: NodeSpecKind): SpecDocument {
  if (kind === "gateway") {
    return {
      node_id: node.id,
      public_endpoints: ["127.0.0.1:4433"],
      listeners: [],
      labels: {},
      capacity: { max_agents: 128, max_connections: 4096, max_services: 4096 },
      port_pool: { tcp: [{ min: 20000, max: 20100 }], udp: [{ min: 21000, max: 21100 }] },
      transport: { alpn: "asterferry-data/2", max_streams: 1024, max_frame_bytes: 65536, max_datagram_bytes: 65536, handshake_timeout_seconds: 10, idle_timeout_seconds: 300 },
      obfuscation: { mode: "standard", max_padding_bytes: 0, handshake_shaping: false },
      egress: { enabled: false, max_connections: 0 },
    };
  }
  return {
    node_id: node.id,
    gateway_selector: { match_labels: {} },
    proxies: [],
    routes: [],
    limits: { max_connections: 4096, max_streams: 1024, max_buffer_bytes: 67108864 },
    egress: { enabled: false, max_connections: 0 },
    logging: { level: "info", format: "json" },
  };
}

let specRequestVersion = 0;

function resetDetailState() {
  section.value = "info";
  visited.value = new Set(["info"]);
  specDoc.value = undefined;
  specText.value = "";
  specRevision.value = undefined;
  specKind.value = undefined;
  selectedKind.value = "agent";
  selectedGatewayID.value = "";
  gatewayNodes.value = [];
  gatewayLoading.value = false;
  gatewayError.value = "";
  gatewayRequestVersion++;
  specValid.value = true;
  specError.value = "";
}

async function loadGatewayNodes(nodeID: string) {
  const requestVersion = ++gatewayRequestVersion;
  gatewayLoading.value = true;
  gatewayError.value = "";
  try {
    const result = await listNodes("gateway");
    if (requestVersion !== gatewayRequestVersion || props.node?.id !== nodeID) return;
    gatewayNodes.value = result.items.filter((item) => item.id !== nodeID);
  } catch (caught) {
    if (requestVersion !== gatewayRequestVersion || props.node?.id !== nodeID) return;
    gatewayError.value = describeError(caught);
    gatewayNodes.value = [];
  } finally {
    if (requestVersion === gatewayRequestVersion) gatewayLoading.value = false;
  }
}

async function loadSpec(node: ControllerNode) {
  const requestVersion = ++specRequestVersion;
  specLoading.value = true;
  specError.value = "";
  selectedKind.value = node.spec_kind ?? "agent";
  try {
    const result = await getNodeSpec(node.id);
    if (requestVersion !== specRequestVersion || props.node?.id !== node.id) return;
    specKind.value = result.kind;
    selectedKind.value = result.kind;
    const doc = result.kind === "gateway" ? result.gateway : result.agent;
    specDoc.value = doc;
    selectedGatewayID.value = result.kind === "agent" ? result.agent?.gateway_id ?? "" : "";
    specRevision.value = result.revision;
    specText.value = prettyJson(doc ?? defaultSpec(node, result.kind));
    if (result.kind === "agent") void loadGatewayNodes(node.id);
  } catch (caught) {
    if (requestVersion !== specRequestVersion || props.node?.id !== node.id) return;
    if (caught instanceof ControllerAPIError && caught.status === 404) {
      specDoc.value = undefined;
      specRevision.value = undefined;
      specKind.value = undefined;
      specText.value = prettyJson(defaultSpec(node, selectedKind.value));
      selectedGatewayID.value = "";
      if (selectedKind.value === "agent") void loadGatewayNodes(node.id);
    } else {
      specError.value = describeError(caught);
    }
  } finally {
    if (requestVersion === specRequestVersion) specLoading.value = false;
  }
}

watch(
  () => [props.open, props.node?.id] as const,
  ([open, nodeId], previous) => {
    if (!open || !nodeId || !props.node) return;
    const [wasOpen, previousNodeId] = previous ?? [];
    const opening = wasOpen !== true;
    const changingNode = nodeId !== previousNodeId;
    if (!opening && !changingNode) return;

    const node = props.node;
    resetDetailState();
    void loadSpec(node);
  },
  { immediate: true },
);

function reloadSpec() {
  if (props.node) void loadSpec(props.node);
}

function openInstall() {
  if (!props.node) return;
  installPlatform.value = "linux";
  installArch.value = "amd64";
  installError.value = "";
  copiedInstallCommand.value = false;
  installResult.value = null;
  installOpen.value = true;
}

function changeInstallPlatform() {
  if (installPlatform.value === "windows") installArch.value = "amd64";
}

async function generateInstallCommand() {
  if (!props.node) return;
  installing.value = true;
  installError.value = "";
  try {
    const input = { platform: installPlatform.value, arch: installArch.value };
    installResult.value = await bootstrapNode(props.node.id, input, undefined, newIdempotencyKey());
    copiedInstallCommand.value = false;
  } catch (caught) {
    installError.value = describeError(caught);
  } finally {
    installing.value = false;
  }
}

async function copyInstallCommand() {
  if (!installResult.value) return;
  try {
    await navigator.clipboard.writeText(installResult.value.command);
    copiedInstallCommand.value = true;
    notify.success("安装命令已复制。请尽快执行，命令中的 Token 只在短时间内有效。");
  } catch {
    notify.error("无法访问剪贴板，请手动复制命令。");
  }
}

function selectInstallCommand(event: FocusEvent) {
  (event.target as HTMLTextAreaElement | null)?.select();
}

function select(id: Section) {
  section.value = id;
  const next = new Set(visited.value);
  next.add(id);
  visited.value = next;
}

function changeSpecKind() {
  if (!props.node || specRevision.value !== undefined) return;
  specDoc.value = undefined;
  specText.value = prettyJson(defaultSpec(props.node, selectedKind.value));
  selectedGatewayID.value = "";
  if (selectedKind.value === "agent") {
    void loadGatewayNodes(props.node.id);
  } else {
    gatewayRequestVersion++;
    gatewayNodes.value = [];
    gatewayLoading.value = false;
    gatewayError.value = "";
  }
}

async function saveSpec() {
  if (!props.node) return;
  specSaving.value = true;
  specError.value = "";
  try {
    const document = parseObject(specText.value, "规格") as SpecDocument;
    document.node_id = props.node.id;
    let envelope: ControllerNodeSpecInput;
    if (selectedKind.value === "gateway") {
      envelope = { node_id: props.node.id, kind: "gateway", gateway: document as ControllerGatewaySpecInput };
    } else {
      if (!selectedGatewayID.value) {
        throw new Error("请先选择要绑定的 Gateway 节点。");
      }
      const agent = document as ControllerAgentSpecInput;
      agent.gateway_id = selectedGatewayID.value;
      envelope = { node_id: props.node.id, kind: "agent", agent };
    }
    const result = await putNodeSpec(props.node.id, envelope, specRevision.value, undefined, newIdempotencyKey());
    const doc = result.kind === "gateway" ? result.gateway : result.agent;
    specKind.value = result.kind;
    selectedKind.value = result.kind;
    specDoc.value = doc;
    specRevision.value = result.revision;
    if (doc) specText.value = prettyJson(doc);
    notify.success(`${specKindLabel(result.kind)} 规格 ${props.node.id} 已保存。`);
    emit("changed");
  } catch (caught) {
    specError.value = describeError(caught);
  } finally {
    specSaving.value = false;
  }
}

async function removeSpec() {
  if (!props.node || specRevision.value === undefined) return;
  deletingSpec.value = true;
  try {
    await deleteNodeSpec(props.node.id, specRevision.value, undefined, newIdempotencyKey());
    notify.success(`规格 ${props.node.id} 已删除，已重置为默认草稿。`);
    confirmDeleteSpec.value = false;
    specDoc.value = undefined;
    specKind.value = undefined;
    specRevision.value = undefined;
    specText.value = prettyJson(defaultSpec(props.node, selectedKind.value));
    emit("changed");
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    deletingSpec.value = false;
  }
}

async function runAction(action: "drain" | "reconnect" | "resync") {
  if (!props.node) return;
  try {
    const result = await nodeAction(props.node.id, action, undefined, newIdempotencyKey());
    notify.success(`${props.node.id}：${action} ${result.state}。`);
    emit("changed");
  } catch (caught) {
    notify.error(describeError(caught));
  }
}
</script>

<template>
  <DrawerPanel :open="open" :title="node ? `${node.name} · ${specKindLabel(specKind || undefined)}` : ''" @close="emit('close')">
    <div v-if="node" class="drawer-layout">
      <nav class="section-nav" aria-label="节点详情分段">
        <button
          v-for="item in sections"
          :key="item.id"
          type="button"
          :class="['section-link', { active: section === item.id }]"
          @click="select(item.id)"
        >{{ item.label }}</button>
      </nav>

      <div class="section-content">
        <PanelCard v-show="section === 'info'" title="基本信息">
          <dl class="info-grid">
            <div><dt>Node ID</dt><dd><code>{{ node.id }}</code></dd></div>
            <div><dt>行为规格</dt><dd>{{ specKindLabel(specKind) }}</dd></div>
            <div><dt>证书</dt><dd><StatusPill :tone="certificateTone(node.certificate_state)">{{ node.certificate_state }}</StatusPill></dd></div>
            <div><dt>证书序列号</dt><dd><code>{{ node.certificate_serial || "—" }}</code></dd></div>
            <div><dt>状态</dt><dd><StatusPill :tone="node.enabled ? 'good' : 'neutral'">{{ node.enabled ? "启用" : "停用" }}</StatusPill></dd></div>
            <div><dt>Revision</dt><dd>{{ node.revision }}</dd></div>
            <div><dt>标签</dt><dd><code class="labels-code">{{ prettyJson(node.labels || {}) }}</code></dd></div>
            <div><dt>创建时间</dt><dd>{{ formatTime(node.created_at) }}</dd></div>
            <div><dt>更新时间</dt><dd>{{ formatTime(node.updated_at) }}</dd></div>
          </dl>
          <div v-if="session.canOperate.value" class="action-row">
            <span class="muted action-label">运行时动作</span>
            <button type="button" class="af-button secondary" @click="runAction('resync')">对账</button>
            <button type="button" class="af-button secondary" @click="runAction('reconnect')">重连</button>
            <button type="button" class="af-button secondary" @click="runAction('drain')">排空</button>
          </div>
          <div v-if="session.canAdmin.value" class="action-row">
            <span class="muted action-label">节点安装</span>
            <button type="button" class="af-button secondary" @click="openInstall">生成安装命令</button>
          </div>
        </PanelCard>

        <PanelCard v-show="section === 'spec'">
          <template #actions>
            <button
              v-if="session.canOperate.value && specRevision !== undefined"
              type="button"
              class="af-button danger-text"
              @click="confirmDeleteSpec = true"
            >删除规格</button>
          </template>
          <div v-if="specLoading" class="loading-row"><Spinner :size="18" /></div>
          <template v-else>
            <label class="spec-kind-field">行为类型
              <select v-model="selectedKind" :disabled="specRevision !== undefined || !nodeEnrolled" @change="changeSpecKind">
                <option value="gateway">Gateway</option>
                <option value="agent">Agent</option>
              </select>
            </label>
            <label v-if="selectedKind === 'agent'" class="spec-kind-field">绑定 Gateway
              <select v-model="selectedGatewayID" :disabled="gatewayLoading || !nodeEnrolled">
                <option value="">请选择已注册的 Gateway</option>
                <option v-for="gateway in gatewayNodes" :key="gateway.id" :value="gateway.id">
                  {{ gateway.name }} · {{ gateway.id }}
                </option>
              </select>
            </label>
            <p v-if="selectedKind === 'agent' && gatewayLoading" class="form-note">正在加载已注册的 Gateway…</p>
            <p v-else-if="selectedKind === 'agent' && gatewayError" class="section-error">Gateway 列表加载失败：{{ gatewayError }}</p>
            <p v-else-if="selectedKind === 'agent' && !gatewayNodes.length" class="form-note">暂无可绑定的 Gateway。请先注册另一个 Node，并在其详情中保存 Gateway 规格。</p>
            <p v-if="!nodeEnrolled" class="form-note">节点尚未完成注册。请先在目标机器执行安装注册命令，注册成功后再配置角色。</p>
            <p v-if="specRevision !== undefined" class="form-note">已有规格如需切换行为，请先删除当前规格，再选择新的行为类型保存。</p>
            <JsonEditor
              v-model="specText"
              :rows="18"
              :footer="`revision ${specRevision ?? 'new'} · 保存后节点按新 generation 对账`"
              @validity="specValid = $event"
            />
            <p v-if="specError" class="section-error">{{ specError }}</p>
            <div class="section-foot">
              <button
                type="button"
                class="af-button primary"
                :disabled="specSaveDisabled"
                @click="saveSpec"
              >{{ specSaving ? "保存中…" : "保存规格" }}</button>
            </div>
          </template>
        </PanelCard>

        <PanelCard v-show="section === 'egress'" title="出口策略">
          <template v-if="specKind">
            <EgressForm :kind="activeKind" :node-id="node.id" :revision="specRevision" :initial="specEgress" @saved="reloadSpec" />
          </template>
          <p v-else class="form-note">请先在“规格”中选择 Gateway 或 Agent 并保存，才能配置出口策略。</p>
        </PanelCard>

        <PanelCard v-if="visited.has('proxies')" v-show="section === 'proxies'" title="代理入口">
          <ProxyRouteManager
            kind="proxies"
            :node-id="node.id"
            :revision="specRevision"
            :items="specProxies"
            @changed="reloadSpec"
          />
        </PanelCard>

        <PanelCard v-if="visited.has('routes')" v-show="section === 'routes'" title="路由规则">
          <ProxyRouteManager
            kind="routes"
            :node-id="node.id"
            :revision="specRevision"
            :items="specRoutes"
            @changed="reloadSpec"
          />
        </PanelCard>

        <PanelCard v-if="visited.has('observed')" v-show="section === 'observed'" title="观测状态">
          <ObservedView :node-id="node.id" />
        </PanelCard>

        <PanelCard v-if="visited.has('snapshot')" v-show="section === 'snapshot'" title="期望快照">
          <SnapshotView :node-id="node.id" />
        </PanelCard>
      </div>
    </div>

    <ModalDialog :open="confirmDeleteSpec" title="删除规格" @close="confirmDeleteSpec = false">
      <p class="confirm-text">确定删除 <strong>{{ node?.id }}</strong> 的业务规格吗？节点将失去对应的监听器、代理与路由配置。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="confirmDeleteSpec = false">取消</button>
        <button type="button" class="af-button danger" :disabled="deletingSpec" @click="removeSpec">{{ deletingSpec ? "删除中…" : "确认删除" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="installOpen" title="一键安装注册命令" width="680px" @close="installOpen = false">
      <div class="install-dialog">
        <template v-if="!installResult">
          <div class="install-grid">
            <label>目标平台<select v-model="installPlatform" @change="changeInstallPlatform"><option value="linux">Linux</option><option value="windows">Windows</option></select></label>
            <label>架构<select v-model="installArch"><option value="amd64">amd64</option><option v-if="installPlatform === 'linux'" value="arm64">arm64</option></select></label>
          </div>
          <p class="form-note">安装命令只安装并注册 Node daemon，不预设 Gateway 或 Agent。完成注册后，在“规格”中选择行为并保存。</p>
          <p v-if="installError" class="section-error">{{ installError }}</p>
        </template>
        <template v-else>
          <p class="form-note">Node daemon · {{ installResult.platform }}/{{ installResult.arch }} · v{{ installResult.version }}</p>
          <p class="form-note warning">命令包含一次性注册 Token，有效期至 {{ new Date(installResult.expires_at).toLocaleString() }}。</p>
          <textarea class="install-command" readonly :value="installResult.command" rows="7" @focus="selectInstallCommand" />
          <p class="form-note">请在对应机器的管理员 PowerShell 或 root shell 中执行。</p>
        </template>
      </div>
      <template #footer>
        <button type="button" class="af-button secondary" @click="installOpen = false">关闭</button>
        <button v-if="!installResult" type="button" class="af-button primary" :disabled="installing" @click="generateInstallCommand">{{ installing ? "生成中…" : "生成命令" }}</button>
        <button v-else type="button" class="af-button primary" @click="copyInstallCommand">{{ copiedInstallCommand ? "已复制" : "复制安装命令" }}</button>
      </template>
    </ModalDialog>
  </DrawerPanel>
</template>

<style scoped>
.drawer-layout {
  display: flex;
  align-items: flex-start;
  gap: 16px;
}
.section-nav {
  display: flex;
  flex: 0 0 108px;
  flex-direction: column;
  gap: 2px;
  position: sticky;
  top: 0;
}
.section-link {
  padding: 7px 10px;
  border-radius: 7px;
  color: var(--af-muted);
  font-size: 13px;
  text-align: left;
  transition: color 120ms ease, background 120ms ease;
}
.section-link:hover { color: var(--af-text); background: var(--af-panel-soft); }
.section-link.active {
  color: var(--af-text);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-sm);
  font-weight: 500;
}
.section-content {
  flex: 1 1 auto;
  min-width: 0;
}
.info-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px 20px;
  margin: 0;
}
.info-grid dt {
  color: var(--af-faint);
  font-size: 11px;
}
.info-grid dd {
  margin: 3px 0 0;
  font-size: 13px;
  overflow-wrap: anywhere;
}
.labels-code {
  color: var(--af-muted);
  font-size: 11px;
}
.action-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px solid var(--af-border-soft);
}
.action-label { font-size: 12px; }
.loading-row {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 160px;
}
.section-error {
  margin: 8px 0 0;
  color: var(--af-red);
  font-size: 12px;
}
.spec-kind-field {
  display: flex;
  flex-direction: column;
  gap: 5px;
  margin-bottom: 10px;
  color: var(--af-faint);
  font-size: 12px;
}
.spec-kind-field select { width: 100%; box-sizing: border-box; }
.form-note {
  margin: 8px 0 0;
  color: var(--af-faint);
  font-size: 12px;
  line-height: 1.6;
}
.section-foot {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
.confirm-text {
  margin: 0;
  color: var(--af-muted);
  line-height: 1.7;
}
.install-dialog { display: flex; flex-direction: column; gap: 10px; }
.install-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.install-dialog label { display: flex; flex-direction: column; gap: 5px; color: var(--af-faint); font-size: 12px; }
.install-dialog input, .install-dialog select { width: 100%; box-sizing: border-box; }
.install-command {
  width: 100%;
  box-sizing: border-box;
  resize: vertical;
  padding: 12px;
  border: 1px solid var(--af-border);
  border-radius: 8px;
  background: var(--af-panel-soft);
  color: var(--af-text);
  font: 12px/1.6 ui-monospace, SFMono-Regular, Consolas, monospace;
}
.warning { color: var(--af-amber, #d99b35); }
@media (max-width: 640px) {
  .drawer-layout { flex-direction: column; }
  .section-nav {
    flex-direction: row;
    flex: 0 0 auto;
    width: 100%;
    overflow-x: auto;
    padding-bottom: 4px;
  }
  .section-link { white-space: nowrap; }
  .install-grid { grid-template-columns: 1fr; }
}
</style>
