<script setup lang="ts">
import { computed, ref } from "vue";
import { RouterLink } from "vue-router";
import PageHeader from "../components/ui/PageHeader.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import MetricCard from "../components/ui/MetricCard.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Spinner from "../components/ui/Spinner.vue";
import {
  listAssignments,
  listAudit,
  listNodes,
  listServices,
  type ControllerAssignment,
  type ControllerAuditRecord,
  type ControllerNode,
  type ControllerService,
} from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { certificateTone, describeError, formatTime } from "../utils/format";

const notify = useNotify();
const loading = ref(true);
const nodes = ref<ControllerNode[]>([]);
const services = ref<ControllerService[]>([]);
const assignments = ref<ControllerAssignment[]>([]);
const audit = ref<ControllerAuditRecord[]>([]);

const configuredCount = computed(() => nodes.value.filter((node) => node.spec_kind).length);
const unconfiguredCount = computed(() => nodes.value.length - configuredCount.value);
const onlineCount = computed(() => nodes.value.filter((node) => node.enabled && node.certificate_state === "active").length);
const appliedCount = computed(() => assignments.value.filter((item) => item.state === "applied").length);

async function load() {
  try {
    const [nodeResult, serviceResult, assignmentResult, auditResult] = await Promise.all([
      listNodes(),
      listServices(),
      listAssignments(),
      listAudit(100),
    ]);
    nodes.value = nodeResult.items;
    services.value = serviceResult.items;
    assignments.value = assignmentResult.items;
    audit.value = auditResult.items;
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
      eyebrow="Control Plane"
      title="运行概览"
      description="所有配置写入 Controller，节点只应用带 generation 和 checksum 的快照。"
    />

    <div v-if="loading" class="loading-row"><Spinner :size="18" /><span class="muted">正在连接 Controller…</span></div>

    <template v-else>
      <div class="metric-grid">
        <MetricCard label="节点" :value="nodes.length" />
        <MetricCard label="在线节点" :value="onlineCount" tone="good" />
        <MetricCard label="已配置行为 / 待配置" :value="`${configuredCount} / ${unconfiguredCount}`" />
        <MetricCard label="服务" :value="services.length" />
        <MetricCard label="已应用调度" :value="appliedCount" />
        <MetricCard label="审计记录" :value="audit.length" />
      </div>

      <div class="panel-grid">
        <PanelCard title="最近节点">
          <template #actions>
            <RouterLink to="/nodes" class="more-link">管理节点 →</RouterLink>
          </template>
          <div v-if="nodes.length" class="compact-list">
            <div v-for="node in nodes.slice(0, 6)" :key="node.id" class="compact-row">
              <span class="row-main">
                <strong>{{ node.name }}</strong>
                <small>{{ node.spec_kind ? node.spec_kind.toUpperCase() : "未配置行为" }} · <code>{{ node.id }}</code></small>
              </span>
              <StatusPill :tone="certificateTone(node.certificate_state)">{{ node.certificate_state }}</StatusPill>
            </div>
          </div>
          <EmptyState v-else title="尚未注册节点" description="在节点页生成 Node 安装命令；目标机器完成注册后才会出现在这里。" />
        </PanelCard>

        <PanelCard title="最近活动">
          <template #actions>
            <RouterLink to="/activity" class="more-link">查看全部 →</RouterLink>
          </template>
          <div v-if="audit.length" class="compact-list">
            <div v-for="item in audit.slice(0, 6)" :key="item.id" class="compact-row">
              <span class="row-main">
                <strong>{{ item.action }} · {{ item.resource }}</strong>
                <small>{{ item.actor }}<template v-if="item.resource_id"> · {{ item.resource_id }}</template></small>
              </span>
              <time class="row-time">{{ formatTime(item.created_at) }}</time>
            </div>
          </div>
          <EmptyState v-else title="暂无活动记录" description="资源的写入、运行时动作和节点事件会统一记录在这里。" />
        </PanelCard>
      </div>
    </template>
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
  align-items: center;
  justify-content: center;
  gap: 10px;
  min-height: 240px;
}
.metric-grid {
  display: grid;
  grid-template-columns: repeat(6, minmax(0, 1fr));
  gap: 12px;
}
.panel-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
}
.more-link {
  color: var(--af-accent);
  font-size: 12px;
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
.row-time {
  flex: 0 0 auto;
  color: var(--af-faint);
  font-size: 11px;
}
@media (max-width: 1100px) {
  .metric-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); }
}
@media (max-width: 900px) {
  .panel-grid { grid-template-columns: 1fr; }
}
@media (max-width: 640px) {
  .metric-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
</style>
