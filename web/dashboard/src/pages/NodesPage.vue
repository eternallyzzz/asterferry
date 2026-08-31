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
  createNode,
  deleteNode,
  listNodes,
  nodeAction,
  updateNode,
  type ControllerNode,
  type CreateNodeInput,
} from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { useSession } from "../session";
import { certificateTone, describeError, newIdempotencyKey, parseLabels, prettyJson, roleLabel } from "../utils/format";

const notify = useNotify();
const session = useSession();
const loading = ref(true);
const nodes = ref<ControllerNode[]>([]);

const drawerNode = ref<ControllerNode | null>(null);
const drawerOpen = ref(false);

const formOpen = ref(false);
const form = ref({ id: "", role: "gateway" as ControllerNode["role"], name: "", labels: "{}", enabled: true });
const editID = ref("");
const editRevision = ref(0);
const formError = ref("");
const saving = ref(false);
const pendingDelete = ref<ControllerNode | null>(null);
const deleting = ref(false);

async function load() {
  try {
    const result = await listNodes();
    nodes.value = result.items;
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
  form.value = { id: "", role: "gateway", name: "", labels: "{}", enabled: true };
  editID.value = "";
  editRevision.value = 0;
  formError.value = "";
  formOpen.value = true;
}

function openEdit(node: ControllerNode) {
  form.value = { id: node.id, role: node.role, name: node.name, labels: prettyJson(node.labels || {}), enabled: node.enabled };
  editID.value = node.id;
  editRevision.value = node.revision;
  formError.value = "";
  formOpen.value = true;
}

async function save() {
  saving.value = true;
  formError.value = "";
  try {
    const input: CreateNodeInput = {
      id: form.value.id.trim(),
      role: form.value.role,
      name: form.value.name.trim(),
      labels: parseLabels(form.value.labels),
      enabled: form.value.enabled,
    };
    if (editID.value) {
      await updateNode(editID.value, { role: input.role, name: input.name, labels: input.labels, enabled: input.enabled }, editRevision.value, undefined, newIdempotencyKey());
      notify.success(`节点 ${editID.value} 已更新。`);
    } else {
      await createNode(input, undefined, newIdempotencyKey());
      notify.success(`节点 ${input.id} 已创建。`);
    }
    formOpen.value = false;
    await refresh();
  } catch (caught) {
    formError.value = describeError(caught);
  } finally {
    saving.value = false;
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
      description="身份由 Admin 管理，Gateway/Agent 业务规格由 Operator 管理；点击行查看详情。"
    >
      <template #actions>
        <button v-if="session.canAdmin.value" type="button" class="af-button primary" @click="openCreate">注册节点</button>
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
          <EmptyState title="暂无节点" description="注册第一个 Gateway 或 Agent，然后在管理页签发 enrollment token 完成接入。" />
        </template>
      </DataTable>
    </PanelCard>

    <NodeDetailDrawer :open="drawerOpen" :node="drawerNode" @close="drawerOpen = false" @changed="refresh" />

    <ModalDialog :open="formOpen" :title="editID ? '编辑节点' : '注册节点'" width="480px" @close="formOpen = false">
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
        <FormField label="名称">
          <input v-model="form.name" required placeholder="East Gateway" />
        </FormField>
        <FormField label="标签 JSON">
          <textarea v-model="form.labels" rows="3" spellcheck="false" placeholder='{"region":"east"}' />
        </FormField>
        <label class="check-label">
          <input v-model="form.enabled" type="checkbox" /> 启用节点
        </label>
        <p v-if="formError" class="form-error">{{ formError }}</p>
        <p v-if="editID" class="form-note">If-Match revision: {{ editRevision }}。并发修改会被 Controller 拒绝。</p>
        <p v-else class="form-note">注册后在「管理」页创建 enrollment token，再运行节点 enroll。</p>
      </form>
      <template #footer>
        <button type="button" class="af-button secondary" @click="formOpen = false">取消</button>
        <button type="button" class="af-button primary" :disabled="saving" @click="save">{{ saving ? "保存中…" : editID ? "保存节点" : "注册节点" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingDelete)" title="删除节点" @close="pendingDelete = null">
      <p class="confirm-text">确定删除节点 <strong>{{ pendingDelete?.id }}</strong> 吗？其规格、调度与快照会一并失效。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingDelete = null">取消</button>
        <button type="button" class="af-button danger" :disabled="deleting" @click="confirmDelete">{{ deleting ? "删除中…" : "确认删除" }}</button>
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
</style>
