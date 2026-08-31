<script setup lang="ts">
import { onMounted, ref } from "vue";
import DataTable from "../ui/DataTable.vue";
import StatusPill from "../ui/StatusPill.vue";
import EmptyState from "../ui/EmptyState.vue";
import Spinner from "../ui/Spinner.vue";
import { ControllerAPIError, getObserved, type ControllerObservedState } from "../../controller-api";
import { useNotify } from "../../composables/useNotify";
import { describeError, formatTime } from "../../utils/format";

const props = defineProps<{ nodeId: string }>();

const notify = useNotify();
const loading = ref(true);
const absent = ref(false);
const state = ref<ControllerObservedState | null>(null);

onMounted(async () => {
  try {
    state.value = await getObserved(props.nodeId);
  } catch (caught) {
    if (caught instanceof ControllerAPIError && caught.status === 404) absent.value = true;
    else notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
  <EmptyState v-else-if="absent" title="暂无观测数据" description="节点上线并应用首个快照后，会周期性上报观测状态。" />
  <div v-else-if="state" class="observed">
    <dl class="info-grid">
      <div><dt>健康</dt><dd><StatusPill :tone="state.healthy ? 'good' : 'bad'">{{ state.healthy ? "健康" : "异常" }}</StatusPill></dd></div>
      <div><dt>降级</dt><dd><StatusPill :tone="state.degraded ? 'warn' : 'neutral'">{{ state.degraded ? "降级" : "正常" }}</StatusPill></dd></div>
      <div><dt>已应用 Generation</dt><dd>{{ state.applied_generation }}</dd></div>
      <div><dt>观测时间</dt><dd>{{ formatTime(state.observed_at) }}</dd></div>
    </dl>

    <div v-if="state.last_error" class="last-error">
      <span class="error-title">最近错误 · {{ state.last_error.code }}<template v-if="state.last_error.path"> · {{ state.last_error.path }}</template></span>
      <p>{{ state.last_error.message }}</p>
    </div>

    <template v-if="state.metrics && Object.keys(state.metrics).length">
      <h3 class="section-title">指标</h3>
      <div class="metric-chips">
        <span v-for="(value, key) in state.metrics" :key="key" class="metric-chip mono">{{ key }}: {{ value }}</span>
      </div>
    </template>

    <template v-if="state.listeners?.length">
      <h3 class="section-title">监听器</h3>
      <DataTable>
        <thead>
          <tr><th>协议</th><th>绑定</th><th>端口</th><th>就绪</th></tr>
        </thead>
        <tbody>
          <tr v-for="listener in state.listeners" :key="`${listener.protocol}-${listener.bind}-${listener.port}`">
            <td>{{ listener.protocol.toUpperCase() }}</td>
            <td><code>{{ listener.bind }}</code></td>
            <td>{{ listener.port }}</td>
            <td><StatusPill :tone="listener.ready ? 'good' : 'warn'">{{ listener.ready ? "就绪" : "未就绪" }}</StatusPill></td>
          </tr>
        </tbody>
      </DataTable>
    </template>

    <template v-if="state.sessions?.length">
      <h3 class="section-title">会话</h3>
      <DataTable>
        <thead>
          <tr><th>ID</th><th>对端</th><th>流数</th><th>建立时间</th></tr>
        </thead>
        <tbody>
          <tr v-for="session in state.sessions" :key="session.id">
            <td><code>{{ session.id }}</code></td>
            <td><code>{{ session.peer_id || "—" }}</code></td>
            <td>{{ session.streams }}</td>
            <td>{{ formatTime(session.started_at) }}</td>
          </tr>
        </tbody>
      </DataTable>
    </template>
  </div>
</template>

<style scoped>
.loading-row {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 160px;
}
.observed {
  display: flex;
  flex-direction: column;
  gap: 14px;
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
}
.last-error {
  padding: 10px 12px;
  border: 1px solid var(--af-red-soft);
  border-radius: var(--af-radius-sm);
  background: var(--af-red-soft);
}
.error-title {
  color: var(--af-red);
  font-size: 12px;
  font-weight: 600;
}
.last-error p {
  margin: 4px 0 0;
  color: var(--af-muted);
  font-size: 12px;
  line-height: 1.6;
}
.section-title {
  margin: 0;
  color: var(--af-muted);
  font-size: 12px;
  font-weight: 600;
}
.metric-chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.metric-chip {
  padding: 3px 8px;
  border: 1px solid var(--af-border);
  border-radius: 6px;
  color: var(--af-muted);
  font-size: 11px;
}
</style>
