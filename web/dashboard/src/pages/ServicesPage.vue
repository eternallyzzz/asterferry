<script setup lang="ts">
import { computed, ref } from "vue";
import PageHeader from "../components/ui/PageHeader.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import DataTable from "../components/ui/DataTable.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import ModalDialog from "../components/ui/ModalDialog.vue";
import FormField from "../components/ui/FormField.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Spinner from "../components/ui/Spinner.vue";
import {
  createService,
  deleteService,
  listNodes,
  listServices,
  updateService,
  type ControllerNode,
  type ControllerService,
} from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { useSession } from "../session";
import { describeError, newIdempotencyKey, parseObject, prettyJson } from "../utils/format";

const notify = useNotify();
const session = useSession();
const loading = ref(true);
const saving = ref(false);
const services = ref<ControllerService[]>([]);
const nodes = ref<ControllerNode[]>([]);

const agentNodes = computed(() => nodes.value.filter((node) => node.role === "agent"));

const emptyForm = () => ({
  id: "",
  agent_id: agentNodes.value[0]?.id || "",
  protocol: "tcp" as ControllerService["protocol"],
  local_target: "127.0.0.1:8080",
  public_bind: "0.0.0.0",
  public_port: "0",
  selector: "{}",
  enabled: true,
});

const formOpen = ref(false);
const form = ref(emptyForm());
const editID = ref("");
const editRevision = ref(0);
const formError = ref("");
const pendingDelete = ref<ControllerService | null>(null);
const deleting = ref(false);

async function load() {
  try {
    const [serviceResult, nodeResult] = await Promise.all([listServices(), listNodes()]);
    services.value = serviceResult.items;
    nodes.value = nodeResult.items;
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
}

const { refresh } = usePolling(load);

function openCreate() {
  form.value = emptyForm();
  editID.value = "";
  editRevision.value = 0;
  formError.value = "";
  formOpen.value = true;
}

function openEdit(service: ControllerService) {
  form.value = {
    id: service.id,
    agent_id: service.agent_id,
    protocol: service.protocol,
    local_target: service.local_target,
    public_bind: service.public_bind,
    public_port: String(service.public_port),
    selector: prettyJson(service.gateway_selector || {}),
    enabled: service.enabled,
  };
  editID.value = service.id;
  editRevision.value = service.revision;
  formError.value = "";
  formOpen.value = true;
}

async function save() {
  saving.value = true;
  formError.value = "";
  try {
    const selector = parseObject(form.value.selector, "gateway_selector");
    const service = {
      id: form.value.id.trim(),
      agent_id: form.value.agent_id.trim(),
      protocol: form.value.protocol,
      local_target: form.value.local_target.trim(),
      public_bind: form.value.public_bind.trim(),
      public_port: Number(form.value.public_port) || 0,
      gateway_selector: selector as { match_labels?: Record<string, string> },
      enabled: form.value.enabled,
    };
    if (editID.value) {
      const { id: _id, ...patch } = service;
      await updateService(editID.value, patch, editRevision.value, undefined, newIdempotencyKey());
      notify.success(`服务 ${editID.value} 已更新。`);
    } else {
      await createService(service, undefined, newIdempotencyKey());
      notify.success(`服务 ${service.id} 已创建。`);
    }
    formOpen.value = false;
    await refresh();
  } catch (caught) {
    formError.value = describeError(caught);
  } finally {
    saving.value = false;
  }
}

async function confirmDelete() {
  if (!pendingDelete.value) return;
  deleting.value = true;
  try {
    await deleteService(pendingDelete.value.id, pendingDelete.value.revision, undefined, newIdempotencyKey());
    notify.success(`服务 ${pendingDelete.value.id} 已删除。`);
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
      eyebrow="Service Catalog"
      title="服务"
      description="创建 TCP/UDP reverse 服务；Controller 会自动选择 Gateway，public_port 为 0 时由端口池分配。"
    >
      <template #actions>
        <button v-if="session.canOperate.value" type="button" class="af-button primary" :disabled="!agentNodes.length" @click="openCreate">创建服务</button>
      </template>
    </PageHeader>

    <PanelCard :title="`服务清单 · ${services.length}`">
      <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
      <DataTable v-else :empty="!services.length">
        <thead>
          <tr><th>服务</th><th>Agent</th><th>协议</th><th>绑定</th><th>状态</th><th v-if="session.canOperate.value">操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="service in services" :key="service.id">
            <td><strong>{{ service.id }}</strong><small>{{ service.local_target }}</small></td>
            <td><code>{{ service.agent_id }}</code></td>
            <td>{{ service.protocol.toUpperCase() }}</td>
            <td>{{ service.public_bind }}:{{ service.public_port || "自动" }}</td>
            <td><StatusPill :tone="service.enabled ? 'good' : 'neutral'">{{ service.enabled ? "启用" : "停用" }}</StatusPill></td>
            <td v-if="session.canOperate.value">
              <div class="row-actions">
                <button type="button" class="af-button text" @click="openEdit(service)">编辑</button>
                <button type="button" class="af-button danger-text" @click="pendingDelete = service">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
        <template #empty>
          <EmptyState
            title="暂无服务"
            :description="agentNodes.length ? '创建服务后，Controller 会按 selector 调度到匹配的 Gateway。' : '先在节点页注册 Agent，再回来创建服务。'"
          />
        </template>
      </DataTable>
    </PanelCard>

    <ModalDialog :open="formOpen" :title="editID ? '编辑服务' : '创建服务'" width="520px" @close="formOpen = false">
      <form class="form-grid" @submit.prevent="save">
        <FormField label="Service ID">
          <input v-model="form.id" :disabled="Boolean(editID)" required placeholder="web-tcp" />
        </FormField>
        <FormField label="Agent">
          <select v-model="form.agent_id" required>
            <option v-for="agent in agentNodes" :key="agent.id" :value="agent.id">{{ agent.name }} ({{ agent.id }})</option>
          </select>
        </FormField>
        <FormField label="协议">
          <select v-model="form.protocol">
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
          </select>
        </FormField>
        <FormField label="公网端口" hint="0 表示由端口池自动分配">
          <input v-model="form.public_port" type="number" min="0" max="65535" />
        </FormField>
        <FormField label="本地目标">
          <input v-model="form.local_target" required placeholder="10.0.0.5:8080" />
        </FormField>
        <FormField label="公网绑定">
          <input v-model="form.public_bind" required placeholder="0.0.0.0" />
        </FormField>
        <FormField label="Gateway selector JSON" class="span-2">
          <textarea v-model="form.selector" rows="3" spellcheck="false" placeholder='{"match_labels":{"region":"east"}}' />
        </FormField>
        <label class="check-label span-2">
          <input v-model="form.enabled" type="checkbox" /> 启用服务
        </label>
        <p v-if="formError" class="form-error span-2">{{ formError }}</p>
      </form>
      <template #footer>
        <button type="button" class="af-button secondary" @click="formOpen = false">取消</button>
        <button type="button" class="af-button primary" :disabled="saving" @click="save">{{ saving ? "保存中…" : editID ? "保存服务" : "创建服务" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingDelete)" title="删除服务" @close="pendingDelete = null">
      <p class="confirm-text">确定删除服务 <strong>{{ pendingDelete?.id }}</strong> 吗？该操作会立即从 Controller 移除配置。</p>
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
  min-height: 200px;
  align-items: center;
}
.row-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}
.form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}
.span-2 { grid-column: span 2; }
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
.confirm-text {
  margin: 0;
  color: var(--af-muted);
  line-height: 1.7;
}
@media (max-width: 640px) {
  .form-grid { grid-template-columns: 1fr; }
  .span-2 { grid-column: span 1; }
}
</style>
