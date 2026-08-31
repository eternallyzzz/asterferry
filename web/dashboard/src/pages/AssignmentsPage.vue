<script setup lang="ts">
import { computed, ref } from "vue";
import PageHeader from "../components/ui/PageHeader.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import DataTable from "../components/ui/DataTable.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Spinner from "../components/ui/Spinner.vue";
import {
  listAssignments,
  listNodes,
  scheduleAgent,
  type ControllerAssignment,
  type ControllerNode,
} from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { useSession } from "../session";
import { describeError, newIdempotencyKey, stateLabel } from "../utils/format";

const notify = useNotify();
const session = useSession();
const loading = ref(true);
const assignments = ref<ControllerAssignment[]>([]);
const nodes = ref<ControllerNode[]>([]);
const expanded = ref<Set<string>>(new Set());
const scheduling = ref("");

const agentNodes = computed(() => nodes.value.filter((node) => node.role === "agent"));

function toggleExpand(id: string) {
  const next = new Set(expanded.value);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  expanded.value = next;
}

async function load() {
  try {
    const [assignmentResult, nodeResult] = await Promise.all([listAssignments(), listNodes()]);
    assignments.value = assignmentResult.items;
    nodes.value = nodeResult.items;
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
}

const { refresh } = usePolling(load);

async function schedule(agent: ControllerNode) {
  scheduling.value = agent.id;
  try {
    await scheduleAgent(agent.id, undefined, newIdempotencyKey());
    notify.success(`已请求为 ${agent.id} 重新调度。`);
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    scheduling.value = "";
  }
}
</script>

<template>
  <div class="page-stack">
    <PageHeader
      eyebrow="Scheduler"
      title="调度与 Assignment"
      description="Controller 根据 selector、容量和端口占用保持健康 assignment；Agent 重连后自动对账。"
    />

    <PanelCard :title="`Assignment · ${assignments.length}`">
      <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
      <DataTable v-else :empty="!assignments.length">
        <thead>
          <tr><th>Assignment</th><th>Gateway</th><th>Agent</th><th>服务</th><th>Generation</th><th>状态</th><th>端点</th></tr>
        </thead>
        <tbody>
          <template v-for="item in assignments" :key="item.id">
            <tr>
              <td><strong>{{ item.id }}</strong><small>revision {{ item.revision ?? "—" }}</small></td>
              <td><code>{{ item.gateway_id }}</code></td>
              <td><code>{{ item.agent_id }}</code></td>
              <td>
                <button v-if="item.bindings?.length" type="button" class="af-button text" @click="toggleExpand(item.id)">
                  {{ item.service_ids.length }} 项{{ expanded.has(item.id) ? " ▴" : " ▾" }}
                </button>
                <span v-else>{{ item.service_ids.length }}</span>
              </td>
              <td>{{ item.generation }}</td>
              <td><StatusPill :tone="item.state === 'applied' ? 'good' : 'warn'">{{ stateLabel(item.state) }}</StatusPill></td>
              <td>{{ item.public_endpoint || "—" }}</td>
            </tr>
            <tr v-if="item.bindings?.length && expanded.has(item.id)" class="bindings-row">
              <td colspan="7">
                <div class="bindings">
                  <span v-for="binding in item.bindings" :key="`${binding.service_id}-${binding.port}`" class="binding-chip mono">
                    {{ binding.service_id }} · {{ binding.protocol.toUpperCase() }} {{ binding.bind }}:{{ binding.port }}
                  </span>
                </div>
              </td>
            </tr>
          </template>
        </tbody>
        <template #empty>
          <EmptyState title="暂无 assignment" description="创建服务并为 Agent 点击「重新调度」后，Controller 会生成 assignment。" />
        </template>
      </DataTable>
    </PanelCard>

    <PanelCard title="Agent 调度">
      <template #actions>
        <span>运行时动作需要 Operator 权限</span>
      </template>
      <div v-if="agentNodes.length" class="compact-list">
        <div v-for="agent in agentNodes" :key="agent.id" class="compact-row">
          <span class="row-main">
            <strong>{{ agent.name }}</strong>
            <small><code>{{ agent.id }}</code></small>
          </span>
          <button
            type="button"
            class="af-button secondary"
            :disabled="!session.canOperate.value || scheduling === agent.id"
            @click="schedule(agent)"
          >{{ scheduling === agent.id ? "调度中…" : "重新调度" }}</button>
        </div>
      </div>
      <EmptyState v-else title="暂无 Agent" description="注册 Agent 节点后即可在此触发重新调度。" />
    </PanelCard>
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
.bindings-row td {
  background: var(--af-panel-soft);
  padding: 10px 12px;
}
.bindings {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.binding-chip {
  padding: 3px 8px;
  border: 1px solid var(--af-border);
  border-radius: 6px;
  color: var(--af-muted);
  font-size: 11px;
}
.compact-list {
  display: flex;
  flex-direction: column;
}
.compact-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 0;
  border-bottom: 1px solid var(--af-border-soft);
}
.compact-row:last-child { border-bottom: 0; }
.row-main {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 2px;
}
.row-main strong { font-size: 13px; font-weight: 600; }
.row-main small { color: var(--af-faint); font-size: 11px; }
</style>
