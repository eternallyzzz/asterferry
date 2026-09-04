<script setup lang="ts">
import { computed, ref } from "vue";
import PageHeader from "../components/ui/PageHeader.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import DataTable from "../components/ui/DataTable.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Spinner from "../components/ui/Spinner.vue";
import { listEvents, type ControllerAuditRecord } from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { describeError, formatTime, prettyJson } from "../utils/format";

type Segment = "all" | "event" | "audit";

const notify = useNotify();
const loading = ref(true);
const items = ref<ControllerAuditRecord[]>([]);
const segment = ref<Segment>("all");

const segments: Array<{ id: Segment; label: string }> = [
  { id: "all", label: "全部" },
  { id: "event", label: "事件" },
  { id: "audit", label: "审计" },
];

function isEvent(item: ControllerAuditRecord): boolean {
  return item.resource === "event" || item.action.startsWith("event:");
}

const filtered = computed(() => {
  const sorted = [...items.value].sort((a, b) => b.id - a.id);
  if (segment.value === "event") return sorted.filter(isEvent);
  if (segment.value === "audit") return sorted.filter((item) => !isEvent(item));
  return sorted;
});

function actionText(item: ControllerAuditRecord): string {
  return item.action.replace(/^event:/, "");
}

async function load() {
  try {
    const result = await listEvents(200);
    items.value = result.items;
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
}

usePolling(load);
</script>

<template>
  <div class="page-stack">
    <PageHeader
      eyebrow="Activity"
      title="事件与审计"
      description="资源写入、运行时动作和节点事件统一记录在 Controller SQLite，按时间倒序。"
    />

    <PanelCard>
      <div class="toolbar">
        <div class="segmented" role="tablist">
          <button
            v-for="item in segments"
            :key="item.id"
            type="button"
            :class="['segment', { active: segment === item.id }]"
            role="tab"
            :aria-selected="segment === item.id"
            @click="segment = item.id"
          >{{ item.label }}</button>
        </div>
        <span class="muted count">{{ filtered.length }} 条</span>
      </div>

      <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
      <DataTable v-else :empty="!filtered.length">
        <thead>
          <tr><th>时间</th><th>主体</th><th>动作</th><th>资源</th><th>资源 ID</th><th>详情</th></tr>
        </thead>
        <tbody>
          <tr v-for="item in filtered" :key="item.id">
            <td class="time-cell">{{ formatTime(item.created_at) }}</td>
            <td>{{ item.actor || "—" }}</td>
            <td>
              <StatusPill :tone="isEvent(item) ? 'neutral' : 'good'">{{ actionText(item) }}</StatusPill>
            </td>
            <td>{{ item.resource }}</td>
            <td><code>{{ item.resource_id || "—" }}</code></td>
            <td>
              <details v-if="item.attributes && Object.keys(item.attributes).length" class="attrs">
                <summary>属性</summary>
                <pre class="mono">{{ prettyJson(item.attributes) }}</pre>
              </details>
              <span v-else class="muted">—</span>
            </td>
          </tr>
        </tbody>
        <template #empty>
          <EmptyState title="暂无记录" description="切换上方分段可查看事件或审计；新记录会随轮询自动出现。" />
        </template>
      </DataTable>
    </PanelCard>
  </div>
</template>

<style scoped>
.page-stack {
  display: flex;
  flex-direction: column;
  gap: 20px;
}
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 16px;
}
.segmented {
  display: inline-flex;
  gap: 2px;
  padding: 3px;
  border: 1px solid var(--af-border);
  border-radius: 999px;
  background: var(--af-panel-soft);
}
.segment {
  padding: 4px 14px;
  border-radius: 999px;
  color: var(--af-muted);
  font-size: 12px;
  font-weight: 500;
  transition: color 120ms ease, background 120ms ease;
}
.segment:hover { color: var(--af-text); }
.segment.active {
  color: var(--af-text);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-sm);
}
.count { font-size: 12px; }
.loading-row {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 200px;
}
.time-cell {
  color: var(--af-muted);
  font-size: 12px;
  white-space: nowrap;
}
.attrs summary {
  color: var(--af-accent);
  cursor: pointer;
  font-size: 12px;
}
.attrs pre {
  max-width: 360px;
  max-height: 180px;
  overflow: auto;
  margin: 6px 0 0;
  padding: 8px;
  border-radius: 6px;
  background: var(--af-panel-soft);
  color: var(--af-muted);
  font-size: 11px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
