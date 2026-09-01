<script setup lang="ts">
import { ref } from "vue";
import PageHeader from "../components/ui/PageHeader.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import DataTable from "../components/ui/DataTable.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import ModalDialog from "../components/ui/ModalDialog.vue";
import FormField from "../components/ui/FormField.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Spinner from "../components/ui/Spinner.vue";
import NodeDetailDrawer from "./NodeDetailDrawer.vue";
import {
	createNodeInstallation,
  deleteNode,
  deleteNodeInstallation,
  listNodeInstallations,
  listNodes,
  nodeAction,
  reissueNodeInstallation,
  updateNode,
	type ControllerNode,
	type NodeBootstrapResponse,
	type PendingNodeInstallation,
} from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { useSession } from "../session";
import { certificateTone, describeError, formatTime, newIdempotencyKey, parseLabels, prettyJson, roleLabel } from "../utils/format";
import { buildGatewayBootstrapSpec } from "../node-bootstrap";

const notify = useNotify();
const session = useSession();
const loading = ref(true);
const nodes = ref<ControllerNode[]>([]);
const pendingInstallations = ref<PendingNodeInstallation[]>([]);

const drawerNode = ref<ControllerNode | null>(null);
const drawerOpen = ref(false);

const formOpen = ref(false);
const form = ref({ id: "", role: "gateway" as ControllerNode["role"], name: "", labels: "{}", enabled: true, platform: "linux" as "linux" | "windows", arch: "amd64" as "amd64" | "arm64", publicEndpoint: "", tcpPool: "28080-28999", udpPool: "28080-28999" });
const editID = ref("");
const editRevision = ref(0);
const formError = ref("");
const saving = ref(false);
const pendingDelete = ref<ControllerNode | null>(null);
const pendingInstallationDelete = ref<PendingNodeInstallation | null>(null);
const deleting = ref(false);
const installOpen = ref(false);
const installResult = ref<NodeBootstrapResponse | null>(null);
const copiedInstallCommand = ref(false);

async function load() {
  try {
    const [result, pending] = await Promise.all([listNodes(), listNodeInstallations()]);
    nodes.value = result.items;
    pendingInstallations.value = pending.items;
    // 抽屉打开时同步节点最新状态（证书、启用位等会随对账变化）。
    if (drawerNode.value) {
      const fresh = result.items.find((node) => node.id === drawerNode.value?.id);
      if (fresh) drawerNode.value = fresh;
    }
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
}

const { refresh } = usePolling(load);

function openDrawer(node: ControllerNode) {
  drawerNode.value = node;
  drawerOpen.value = true;
}

function openCreate() {
	form.value = { id: "", role: "gateway", name: "", labels: "{}", enabled: true, platform: "linux", arch: "amd64", publicEndpoint: "", tcpPool: "28080-28999", udpPool: "28080-28999" };
  editID.value = "";
  editRevision.value = 0;
  formError.value = "";
  formOpen.value = true;
}

function openEdit(node: ControllerNode) {
  form.value = { id: node.id, role: node.role, name: node.name, labels: prettyJson(node.labels || {}), enabled: node.enabled, platform: "linux", arch: "amd64", publicEndpoint: "", tcpPool: "28080-28999", udpPool: "28080-28999" };
  editID.value = node.id;
  editRevision.value = node.revision;
  formError.value = "";
  formOpen.value = true;
}

async function save() {
  saving.value = true;
  formError.value = "";
  try {
    const labels = parseLabels(form.value.labels);
    const nodeID = form.value.id.trim();
    const name = form.value.name.trim();
    if (editID.value) {
      await updateNode(editID.value, { role: form.value.role, name, labels, enabled: form.value.enabled }, editRevision.value, undefined, newIdempotencyKey());
      notify.success(`节点 ${editID.value} 已更新。`);
    } else {
      const bootstrap = await createNodeInstallation({
        node_id: nodeID,
        role: form.value.role,
        name,
        labels,
        enabled: form.value.enabled,
        platform: form.value.platform,
        arch: form.value.arch,
        ...(form.value.role === "gateway" ? { gateway_spec: buildGatewayBootstrapSpec({ id: nodeID, labels }, form.value.publicEndpoint, form.value.tcpPool, form.value.udpPool) } : {}),
      }, undefined, newIdempotencyKey());
      notify.success(`安装任务 ${nodeID} 已创建；节点执行命令后才会注册。`);
      formOpen.value = false;
      await refresh();
      installResult.value = bootstrap;
      copiedInstallCommand.value = false;
      installOpen.value = true;
      return;
    }
    formOpen.value = false;
    await refresh();
  } catch (caught) {
    formError.value = describeError(caught);
  } finally {
    saving.value = false;
  }
}

async function reissueInstallation(item: PendingNodeInstallation) {
  try {
    const bootstrap = await reissueNodeInstallation(item.node_id, undefined, newIdempotencyKey());
    installResult.value = bootstrap;
    copiedInstallCommand.value = false;
    installOpen.value = true;
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  }
}

async function confirmPendingInstallationDelete() {
  if (!pendingInstallationDelete.value) return;
  deleting.value = true;
  try {
    await deleteNodeInstallation(pendingInstallationDelete.value.node_id, undefined, newIdempotencyKey());
    notify.success(`安装任务 ${pendingInstallationDelete.value.node_id} 已取消。`);
    pendingInstallationDelete.value = null;
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    deleting.value = false;
  }
}

function changePlatform() {
  if (form.value.platform === "windows") form.value.arch = "amd64";
}

function selectInstallCommand(event: FocusEvent) {
  (event.target as HTMLTextAreaElement | null)?.select();
}

async function copyInstallCommand() {
  if (!installResult.value) return;
  try {
    await navigator.clipboard.writeText(installResult.value.command);
    copiedInstallCommand.value = true;
    notify.success("安装命令已复制。请尽快执行，命令中的 Token 只在短时间内有效。" );
  } catch {
    notify.error("无法访问剪贴板，请手动复制命令。" );
  }
}

async function runAction(node: ControllerNode, action: "drain" | "resync") {
  try {
    const result = await nodeAction(node.id, action, undefined, newIdempotencyKey());
    notify.success(`${node.id}：${action} ${result.state}。`);
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  }
}

async function confirmDelete() {
  if (!pendingDelete.value) return;
  deleting.value = true;
  try {
    await deleteNode(pendingDelete.value.id, pendingDelete.value.revision, undefined, newIdempotencyKey());
    notify.success(`节点 ${pendingDelete.value.id} 已删除。`);
    pendingDelete.value = null;
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    deleting.value = false;
  }
}
</script>

<template>
  <div class="page-stack">
    <PageHeader
      eyebrow="Nodes"
      title="节点"
      description="先创建安装任务，节点执行命令并连接 Controller 后才会出现在这里。"
    >
      <template #actions>
        <button v-if="session.canAdmin.value" type="button" class="af-button primary" @click="openCreate">生成安装命令</button>
      </template>
    </PageHeader>

    <PanelCard :title="`节点清单 · ${nodes.length}`">
      <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
      <DataTable v-else :empty="!nodes.length">
        <thead>
          <tr><th>节点</th><th>角色</th><th>证书</th><th>状态</th><th>Revision</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="node in nodes" :key="node.id" class="clickable" @click="openDrawer(node)">
            <td><strong>{{ node.name }}</strong><small><code>{{ node.id }}</code></small></td>
            <td>{{ roleLabel(node.role) }}</td>
            <td><StatusPill :tone="certificateTone(node.certificate_state)">{{ node.certificate_state }}</StatusPill></td>
            <td><StatusPill :tone="node.enabled ? 'good' : 'neutral'">{{ node.enabled ? "启用" : "停用" }}</StatusPill></td>
            <td>{{ node.revision }}</td>
            <td>
              <div class="row-actions" @click.stop>
                <button type="button" class="af-button text" @click="openDrawer(node)">详情</button>
                <button v-if="session.canAdmin.value" type="button" class="af-button text" @click="openEdit(node)">编辑</button>
                <template v-if="session.canOperate.value">
                  <button type="button" class="af-button text" @click="runAction(node, 'resync')">对账</button>
                  <button type="button" class="af-button text" @click="runAction(node, 'drain')">排空</button>
                </template>
                <button v-if="session.canAdmin.value" type="button" class="af-button danger-text" @click="pendingDelete = node">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
        <template #empty>
          <EmptyState title="暂无已注册节点" description="先生成安装命令，并在目标机器执行一次。" />
        </template>
      </DataTable>
    </PanelCard>

    <PanelCard v-if="pendingInstallations.length" :title="`待安装任务 · ${pendingInstallations.length}`">
      <DataTable>
        <thead><tr><th>节点</th><th>角色</th><th>平台</th><th>有效期</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="item in pendingInstallations" :key="item.node_id">
            <td><strong>{{ item.name }}</strong><small><code>{{ item.node_id }}</code></small></td>
            <td>{{ roleLabel(item.role) }}</td>
            <td>{{ item.platform }}/{{ item.arch }}</td>
            <td>{{ formatTime(item.expires_at) }}</td>
            <td>
              <div class="row-actions">
                <button v-if="session.canAdmin.value" type="button" class="af-button text" @click="reissueInstallation(item)">重新生成</button>
                <button v-if="session.canAdmin.value" type="button" class="af-button danger-text" @click="pendingInstallationDelete = item">取消</button>
              </div>
            </td>
          </tr>
        </tbody>
      </DataTable>
    </PanelCard>

    <NodeDetailDrawer :open="drawerOpen" :node="drawerNode" @close="drawerOpen = false" @changed="refresh" />

    <ModalDialog :open="formOpen" :title="editID ? '编辑节点' : '生成节点安装命令'" width="480px" @close="formOpen = false">
      <form class="form-stack" @submit.prevent="save">
        <FormField label="Node ID">
          <input v-model="form.id" :disabled="Boolean(editID)" required placeholder="gw-east" />
        </FormField>
        <FormField label="角色">
          <select v-model="form.role" :disabled="Boolean(editID)">
            <option value="gateway">Gateway</option>
            <option value="agent">Agent</option>
          </select>
        </FormField>
        <template v-if="!editID">
          <FormField label="目标平台">
            <select v-model="form.platform" @change="changePlatform">
              <option value="linux">Linux</option>
              <option value="windows">Windows</option>
            </select>
          </FormField>
          <FormField label="架构">
            <select v-model="form.arch">
              <option value="amd64">amd64</option>
              <option v-if="form.platform === 'linux'" value="arm64">arm64</option>
            </select>
          </FormField>
        </template>
        <FormField label="名称">
          <input v-model="form.name" required placeholder="East Gateway" />
        </FormField>
        <FormField label="标签 JSON">
          <textarea v-model="form.labels" rows="3" spellcheck="false" placeholder='{"region":"east"}' />
        </FormField>
        <template v-if="!editID && form.role === 'gateway'">
          <FormField label="Gateway 数据面地址" hint="Agent 连接 Gateway 的公网/NAT 地址，例如 gateway.example.com:4433；不是 Controller 地址">
            <input v-model="form.publicEndpoint" required placeholder="gateway.example.com:4433" />
          </FormField>
          <div />
          <FormField label="TCP 端口池" hint="逗号分隔端口或范围">
            <input v-model="form.tcpPool" placeholder="28080-28999" />
          </FormField>
          <FormField label="UDP 端口池" hint="逗号分隔端口或范围">
            <input v-model="form.udpPool" placeholder="28080-28999" />
          </FormField>
        </template>
        <label class="check-label">
          <input v-model="form.enabled" type="checkbox" /> 启用节点
        </label>
        <p v-if="formError" class="form-error">{{ formError }}</p>
        <p v-if="editID" class="form-note">If-Match revision: {{ editRevision }}。并发修改会被 Controller 拒绝。</p>
        <p v-else class="form-note">这里只创建待安装任务，不会提前注册节点。Agent 使用默认规格；Gateway 的地址和端口池属于数据面预配置。</p>
      </form>
      <template #footer>
        <button type="button" class="af-button secondary" @click="formOpen = false">取消</button>
        <button type="button" class="af-button primary" :disabled="saving" @click="save">{{ saving ? "保存中…" : editID ? "保存节点" : "生成安装命令" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="installOpen" title="一键安装注册命令" width="680px" @close="installOpen = false">
      <div v-if="installResult" class="install-dialog">
        <p class="form-note">{{ installResult.role.toUpperCase() }} · {{ installResult.platform }}/{{ installResult.arch }} · v{{ installResult.version }}</p>
        <p class="form-note warning">命令包含一次性注册 Token，有效期至 {{ new Date(installResult.expires_at).toLocaleString() }}。不要发到公开聊天或日志中。</p>
        <textarea class="install-command" readonly :value="installResult.command" rows="7" @focus="selectInstallCommand" />
        <p class="form-note">在对应机器的管理员 PowerShell 或 root shell 中执行。执行前节点不会出现在正式节点列表；Enroll 成功后才会自动上线。</p>
      </div>
      <template #footer>
        <button type="button" class="af-button secondary" @click="installOpen = false">稍后复制</button>
        <button type="button" class="af-button primary" @click="copyInstallCommand">{{ copiedInstallCommand ? "已复制" : "复制安装命令" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingDelete)" title="删除节点" @close="pendingDelete = null">
      <p class="confirm-text">确定删除节点 <strong>{{ pendingDelete?.id }}</strong> 吗？其规格、调度与快照会一并失效。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingDelete = null">取消</button>
        <button type="button" class="af-button danger" :disabled="deleting" @click="confirmDelete">{{ deleting ? "删除中…" : "确认删除" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingInstallationDelete)" title="取消待安装任务" @close="pendingInstallationDelete = null">
      <p class="confirm-text">确定取消 <strong>{{ pendingInstallationDelete?.node_id }}</strong> 的待安装任务吗？已有命令将立即失效。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingInstallationDelete = null">取消</button>
        <button type="button" class="af-button danger" :disabled="deleting" @click="confirmPendingInstallationDelete">{{ deleting ? "取消中…" : "确认取消" }}</button>
      </template>
    </ModalDialog>
  </div>
</template>

<style scoped>
.page-stack {
  display: flex;
  flex-direction: column;
  gap: 20px;
}
.loading-row {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 200px;
}
.clickable { cursor: pointer; }
.row-actions {
  display: flex;
  align-items: center;
  gap: 2px;
}
.form-stack {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.check-label {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--af-muted);
  font-size: 13px;
}
.form-error {
  margin: 0;
  color: var(--af-red);
  font-size: 12px;
}
.form-note {
  margin: 0;
  color: var(--af-faint);
  font-size: 12px;
  line-height: 1.6;
}
.confirm-text {
  margin: 0;
  color: var(--af-muted);
  line-height: 1.7;
}
.install-dialog { display: flex; flex-direction: column; gap: 10px; }
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
</style>
