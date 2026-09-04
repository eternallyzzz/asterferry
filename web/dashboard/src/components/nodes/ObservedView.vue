<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import DataTable from "../ui/DataTable.vue";
import StatusPill from "../ui/StatusPill.vue";
import EmptyState from "../ui/EmptyState.vue";
import Spinner from "../ui/Spinner.vue";
import { ControllerAPIError, getObserved, getNodeRuntimeConnections, getRuntimeSettings, listRuntimeEvents, listRuntimeTraffic, runtimeConnectionAction, runtimeStreamURL, type ControllerObservedState, type RuntimeConnection, type RuntimeEventRecord, type RuntimeTrafficRollup } from "../../controller-api";
import { useNotify } from "../../composables/useNotify";
import { usePolling } from "../../composables/usePolling";
import { useSession } from "../../session";
import { describeError, formatTime } from "../../utils/format";

const props = defineProps<{ nodeId: string }>();

const notify = useNotify();
const session = useSession();
const loading = ref(true);
const absent = ref(false);
const state = ref<ControllerObservedState | null>(null);
const runtimeConnections = ref<RuntimeConnection[]>([]);
const runtimeEvents = ref<RuntimeEventRecord[]>([]);
const runtimeTraffic = ref<RuntimeTrafficRollup[]>([]);
const advancedOperationsEnabled = ref(false);
const runtimeLoading = ref(true);
const runtimeActionBusy = ref("");
const limitRate = ref("1048576");
const limitTTL = ref("3600");

async function load() {
  try {
    const [observed, connections, settings, events, traffic] = await Promise.all([
      getObserved(props.nodeId),
      getNodeRuntimeConnections(props.nodeId),
      getRuntimeSettings(),
      listRuntimeEvents(props.nodeId, undefined, 100),
      listRuntimeTraffic(props.nodeId),
    ]);
    state.value = observed;
    runtimeConnections.value = connections.items;
    advancedOperationsEnabled.value = settings.advanced_operations_enabled;
    runtimeEvents.value = events.items;
    runtimeTraffic.value = traffic.items;
  } catch (caught) {
    if (caught instanceof ControllerAPIError && caught.status === 404) absent.value = true;
    else notify.error(describeError(caught));
  } finally {
    loading.value = false;
    runtimeLoading.value = false;
  }
}

async function refreshRuntime() {
  try {
    const [connections, settings, events, traffic] = await Promise.all([getNodeRuntimeConnections(props.nodeId), getRuntimeSettings(), listRuntimeEvents(props.nodeId, undefined, 100), listRuntimeTraffic(props.nodeId)]);
    runtimeConnections.value = connections.items;
    advancedOperationsEnabled.value = settings.advanced_operations_enabled;
    runtimeEvents.value = events.items;
    runtimeTraffic.value = traffic.items;
  } catch (caught) {
    // The normal observed state should remain useful when only the optional
    // runtime endpoint is unavailable.
    if (!(caught instanceof ControllerAPIError && caught.status === 404)) notify.error(describeError(caught));
  } finally {
    runtimeLoading.value = false;
  }
}

const { refresh } = usePolling(load, 10_000);
let runtimeEventSource: EventSource | undefined;
onMounted(() => {
  runtimeEventSource = new EventSource(runtimeStreamURL(props.nodeId));
  runtimeEventSource.addEventListener("runtime", () => void refreshRuntime());
  runtimeEventSource.onerror = () => runtimeEventSource?.close();
});
onUnmounted(() => runtimeEventSource?.close());

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "—";
  if (value < 1024) return `${Math.round(value)} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
  if (value < 1024 * 1024 * 1024) return `${(value / (1024 * 1024)).toFixed(1)} MiB`;
  return `${(value / (1024 * 1024 * 1024)).toFixed(1)} GiB`;
}

const activeRuntimeConnections = computed(() => runtimeConnections.value.filter((item) => item.state === "active").length);
const runtimeBytesIn = computed(() => runtimeConnections.value.reduce((total, item) => total + (Number.isFinite(item.bytes_in) ? item.bytes_in : 0), 0));
const runtimeBytesOut = computed(() => runtimeConnections.value.reduce((total, item) => total + (Number.isFinite(item.bytes_out) ? item.bytes_out : 0), 0));

function runtimeEventPayload(event: RuntimeEventRecord): Record<string, unknown> {
  return event.payload && typeof event.payload === "object" ? event.payload : {};
}

function runtimeEventLabel(event: RuntimeEventRecord): string {
  const type = runtimeEventPayload(event).type;
  switch (type) {
    case "opened": return "连接进入";
    case "closed": return "连接断开";
    case "rejected": return "连接拒绝";
    case "rate_limited": return "已限速";
    case "updated": return "连接更新";
    default: return event.type;
  }
}

function runtimeEventMessage(event: RuntimeEventRecord): string {
  const message = runtimeEventPayload(event).message;
  return typeof message === "string" && message ? message : "—";
}

function runtimeTrafficLabel(item: RuntimeTrafficRollup): string {
  return item.assignment_id || item.service_id || "节点级";
}

async function disconnect(connection: RuntimeConnection) {
  runtimeActionBusy.value = connection.id;
  try {
    await runtimeConnectionAction(props.nodeId, connection.id, { action: "disconnect" }, undefined);
    notify.success(`连接 ${connection.id} 已提交断开。`);
    await refreshRuntime();
  } catch (caught) { notify.error(describeError(caught)); }
  finally { runtimeActionBusy.value = ""; }
}

async function rateLimit(connection: RuntimeConnection) {
  const bytes = Math.max(1, Math.floor(Number(limitRate.value) || 0));
  const ttl = Math.max(60, Math.min(86400, Math.floor(Number(limitTTL.value) || 3600)));
  runtimeActionBusy.value = connection.id;
  try {
    await runtimeConnectionAction(props.nodeId, connection.id, { action: "rate_limit", direction: "both", bytes_per_second: bytes, burst_bytes: Math.max(bytes, bytes * 2), ttl_seconds: ttl }, undefined);
    notify.success(`连接 ${connection.id} 已提交限速。`);
    await refreshRuntime();
  } catch (caught) { notify.error(describeError(caught)); }
  finally { runtimeActionBusy.value = ""; }
}

async function clearLimit(connection: RuntimeConnection) {
  runtimeActionBusy.value = connection.id;
  try {
    await runtimeConnectionAction(props.nodeId, connection.id, { action: "clear_limit" }, undefined);
    notify.success(`连接 ${connection.id} 的限速已提交清除。`);
    await refreshRuntime();
  } catch (caught) { notify.error(describeError(caught)); }
  finally { runtimeActionBusy.value = ""; }
}
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
      <span class="error-title">
        最近错误 · {{ state.last_error.code }}<template v-if="state.last_error.path"> · {{ state.last_error.path }}</template>
        <StatusPill class="error-retryability" :tone="state.last_error.retryable ? 'warn' : 'bad'">
          {{ state.last_error.retryable ? "可重试" : "需处理" }}
        </StatusPill>
      </span>
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

    <section class="runtime-section">
      <div class="runtime-heading">
        <div>
          <h3 class="section-title">实时连接与流量</h3>
          <p class="runtime-note">默认只读展示元数据：来源 IP、协议、对端、目标、累计字节与平均速率；不采集业务载荷。</p>
        </div>
        <StatusPill :tone="advancedOperationsEnabled ? 'warn' : 'neutral'">高级操作 {{ advancedOperationsEnabled ? "已开启" : "已关闭" }}</StatusPill>
      </div>
      <div class="runtime-stats">
        <span><strong>{{ activeRuntimeConnections }}</strong> 活动连接</span>
        <span><strong>{{ formatBytes(runtimeBytesIn) }}</strong> 入站</span>
        <span><strong>{{ formatBytes(runtimeBytesOut) }}</strong> 出站</span>
        <span><strong>{{ runtimeEvents.filter((event) => runtimeEventPayload(event).type === 'opened').length }}</strong> 进入</span>
        <span><strong>{{ runtimeEvents.filter((event) => runtimeEventPayload(event).type === 'closed').length }}</strong> 断开</span>
        <span><strong>{{ runtimeEvents.filter((event) => runtimeEventPayload(event).type === 'rejected').length }}</strong> 拒绝</span>
      </div>
      <div v-if="advancedOperationsEnabled && session.canOperate.value" class="runtime-controls">
        <label>双向限速（B/s）<input v-model="limitRate" type="number" min="1" step="1024" /></label>
        <label>有效期（秒）<input v-model="limitTTL" type="number" min="60" max="86400" /></label>
        <span>断开与限速操作会写入审计日志。</span>
      </div>
      <div v-if="runtimeLoading" class="loading-row"><Spinner :size="18" /></div>
      <EmptyState v-else-if="!runtimeConnections.length" title="暂无实时连接" description="节点产生 AFDP 会话或业务流后，这里会显示连接元数据。" />
      <DataTable v-else>
        <thead>
          <tr><th>状态 / 类型</th><th>来源</th><th>对端 / 归属</th><th>目标</th><th>流量</th><th>时间</th><th v-if="advancedOperationsEnabled && session.canOperate.value">操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="connection in runtimeConnections" :key="connection.id">
            <td><StatusPill :tone="connection.state === 'active' ? 'good' : connection.state === 'unknown' ? 'warn' : 'neutral'">{{ connection.state }}</StatusPill><br /><code>{{ connection.type }} · {{ connection.protocol || "—" }}</code></td>
            <td><code>{{ connection.source_ip || "—" }}{{ connection.source_port ? `:${connection.source_port}` : "" }}</code></td>
            <td><code>{{ connection.peer_node_id || "—" }}</code><br /><span class="runtime-subtext">{{ connection.assignment_id || connection.service_id || "未关联业务" }}</span></td>
            <td><code>{{ connection.target || "—" }}</code></td>
            <td><span class="mono">↓ {{ formatBytes(connection.bytes_in) }} · ↑ {{ formatBytes(connection.bytes_out) }}</span><br /><span class="runtime-subtext">{{ formatBytes(connection.rate_in_bps) }}/s · {{ formatBytes(connection.rate_out_bps) }}/s</span></td>
            <td><span class="runtime-subtext">进入 {{ formatTime(connection.started_at) }}</span><br /><span v-if="connection.ended_at" class="runtime-subtext">离开 {{ formatTime(connection.ended_at) }}</span><span v-else class="runtime-subtext">仍在活动</span><br /><span v-if="connection.close_reason" class="runtime-subtext">{{ connection.close_reason }}</span></td>
            <td v-if="advancedOperationsEnabled && session.canOperate.value"><div class="row-actions"><button type="button" class="af-button danger-text" :disabled="runtimeActionBusy === connection.id" @click="disconnect(connection)">断开</button><button v-if="connection.limit" type="button" class="af-button text" :disabled="runtimeActionBusy === connection.id" @click="clearLimit(connection)">清限速</button><button v-else type="button" class="af-button text" :disabled="runtimeActionBusy === connection.id" @click="rateLimit(connection)">限速</button></div></td>
          </tr>
        </tbody>
      </DataTable>

      <template v-if="runtimeEvents.length">
        <h3 class="section-title">最近连接事件</h3>
        <DataTable>
          <thead><tr><th>时间</th><th>事件</th><th>连接</th><th>说明</th></tr></thead>
          <tbody>
            <tr v-for="event in runtimeEvents.slice(0, 30)" :key="event.event_id">
              <td>{{ formatTime(event.created_at) }}</td>
              <td><StatusPill :tone="runtimeEventPayload(event).type === 'rejected' ? 'warn' : runtimeEventPayload(event).type === 'closed' ? 'neutral' : 'good'">{{ runtimeEventLabel(event) }}</StatusPill></td>
              <td><code>{{ event.connection_id || "节点级" }}</code></td>
              <td>{{ runtimeEventMessage(event) }}</td>
            </tr>
          </tbody>
        </DataTable>
      </template>

      <template v-if="runtimeTraffic.length">
        <h3 class="section-title">分钟流量汇总</h3>
        <DataTable>
          <thead><tr><th>分钟</th><th>归属</th><th>协议</th><th>流量</th><th>事件</th><th>活动峰值</th></tr></thead>
          <tbody>
            <tr v-for="item in runtimeTraffic.slice(0, 30)" :key="`${item.bucket_start}-${item.assignment_id}-${item.service_id}-${item.protocol}`">
              <td>{{ formatTime(item.bucket_start) }}</td>
              <td><code>{{ runtimeTrafficLabel(item) }}</code></td>
              <td>{{ item.protocol || "节点级" }}</td>
              <td class="mono">↓ {{ formatBytes(item.bytes_in) }} · ↑ {{ formatBytes(item.bytes_out) }}</td>
              <td>{{ item.opened }} 进入 · {{ item.closed }} 断开 · {{ item.rejected }} 拒绝 · {{ item.rate_limited }} 限速</td>
              <td>{{ item.active_max }}</td>
            </tr>
          </tbody>
        </DataTable>
      </template>
    </section>
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
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  color: var(--af-red);
  font-size: 12px;
  font-weight: 600;
}
.error-retryability { font-size: 10px; }
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
.runtime-section {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin-top: 4px;
}
.runtime-heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}
.runtime-note,
.runtime-subtext {
  margin: 4px 0 0;
  color: var(--af-faint);
  font-size: 11px;
}
.runtime-stats {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.runtime-stats span {
  padding: 6px 9px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius-sm);
  color: var(--af-muted);
  font-size: 11px;
}
.runtime-stats strong {
  color: var(--af-text);
  font-size: 13px;
}
.runtime-controls {
  display: flex;
  align-items: end;
  flex-wrap: wrap;
  gap: 10px;
  padding: 10px;
  border: 1px solid var(--af-amber-soft);
  border-radius: var(--af-radius-sm);
  background: var(--af-amber-soft);
  color: var(--af-muted);
  font-size: 11px;
}
.runtime-controls label { display: flex; flex-direction: column; gap: 4px; }
.runtime-controls input { width: 110px; }
.row-actions { display: flex; gap: 2px; align-items: center; }
</style>
