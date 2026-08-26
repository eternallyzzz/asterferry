<script setup lang="ts">
import { computed } from "vue";
import { Alert, Button, Card, Collapse, CollapseItem, Space, Tag } from "@arco-design/web-vue";
import { useRouter } from "vue-router";
import { useDashboardContext } from "../dashboard-context";
import { formatDuration, formatRate, stateTone, statusLabel } from "../model";
import Sparkline from "../components/Sparkline.vue";
import TermHelp from "../components/TermHelp.vue";

const { snapshot, trend, events } = useDashboardContext();
const router = useRouter();

const roleLabel = computed(() => snapshot.value?.role === "gateway" ? "Gateway" : "Agent");
const agentCount = computed(() => snapshot.value?.gateway?.agents.length ?? 0);
const mappingCount = computed(() => snapshot.value?.gateway?.mappings.length ?? snapshot.value?.agent?.reverse_mappings.length ?? 0);
const currentInput = computed(() => latest(trend.value.input));
const currentOutput = computed(() => latest(trend.value.output));
const currentErrors = computed(() => latest(trend.value.errors));
const primaryStatus = computed(() => snapshot.value ? statusLabel(snapshot.value) : "加载中");

function latest(points: { value: number }[]): number {
  return points.length ? points[points.length - 1].value : 0;
}

function open(path: string) {
  void router.push(path);
}
</script>

<template>
  <div class="page-stack">
    <Alert v-if="snapshot && !snapshot.ready" type="warning" show-icon class="page-alert">
      当前运行状态为“{{ primaryStatus }}”，请检查节点连接和最近事件。
    </Alert>

    <section v-if="snapshot" class="overview-hero">
      <Card class="hero-card" :bordered="false">
        <div class="status-line">
          <Tag :color="stateTone(snapshot)" bordered>{{ primaryStatus }}</Tag>
          <span class="muted">{{ roleLabel }} runtime</span>
        </div>
        <h2>{{ snapshot.ready ? "一切正常" : "需要关注运行状态" }}</h2>
        <p class="muted">
          Node <code>{{ snapshot.node_id || "local" }}</code>
          · Protocol v{{ snapshot.transport.protocol }}
          · {{ snapshot.transport.obfuscation_mode }}
        </p>
        <Space>
          <Button type="primary" @click="open('/services')">查看内网服务</Button>
          <Button type="outline" @click="open('/help#quick-start')">查看快速开始</Button>
        </Space>
      </Card>
      <Card class="topology-card" :bordered="false">
        <div class="section-kicker">LIVE TOPOLOGY</div>
        <div class="topology">
          <div class="topology-node">
            <span class="topology-icon">⌂</span>
            <strong>内网服务</strong>
            <small>{{ mappingCount }} 个 reverse mapping</small>
          </div>
          <span class="topology-arrow">→</span>
          <div class="topology-node">
            <span class="topology-icon">◎</span>
            <strong>{{ roleLabel }}</strong>
            <small>{{ snapshot.ready ? "在线" : "未就绪" }}</small>
          </div>
          <span class="topology-arrow">→</span>
          <div class="topology-node">
            <span class="topology-icon">◉</span>
            <strong>{{ snapshot.role === "agent" ? "Gateway" : "访客入口" }}</strong>
            <small>{{ snapshot.role === "agent" ? (snapshot.agent?.connected ? "已连接" : "离线") : agentCount + " 个 Agent" }}</small>
          </div>
        </div>
      </Card>
    </section>

    <section v-if="snapshot" class="metric-grid">
      <Card :bordered="false"><div class="mini-stat"><span>连接状态</span><strong>{{ snapshot.role === 'gateway' ? agentCount + ' 个 Agent' : (snapshot.agent?.connected ? '已连接' : '离线') }}</strong></div></Card>
      <Card :bordered="false"><div class="mini-stat"><span>Reverse mappings <TermHelp term="Reverse mapping" description="把内网服务映射到 Gateway 端口的配置。" target="concepts" /></span><strong>{{ mappingCount }}</strong></div></Card>
      <Card :bordered="false"><div class="mini-stat"><span>Inbound <TermHelp term="Inbound" description="Agent 本机监听的代理入口。" target="concepts" /></span><strong>{{ snapshot.agent?.inbounds.length ?? 0 }}</strong></div></Card>
      <Card :bordered="false"><div class="mini-stat"><span>QUIC RTT</span><strong>{{ formatDuration(snapshot.metrics.quic.rtt_microseconds / 1000) }}</strong></div></Card>
      <Card :bordered="false"><div class="mini-stat"><span>流入速率</span><strong class="good-text">{{ formatRate(currentInput) }}</strong></div></Card>
      <Card :bordered="false"><div class="mini-stat"><span>流出速率</span><strong>{{ formatRate(currentOutput) }}</strong></div></Card>
    </section>

    <section v-if="snapshot" class="chart-grid">
      <Card :bordered="false">
        <template #title>流量趋势</template>
        <template #extra><span class="muted">最近 {{ trend.input.length }} 个采样</span></template>
        <div class="legend"><span><i class="legend-in" /> 流入</span><span><i class="legend-out" /> 流出</span></div>
        <Sparkline :first="trend.input" :second="trend.output" label="流量趋势" />
      </Card>
      <Card :bordered="false">
        <template #title>错误趋势</template>
        <template #extra><span class="muted">当前 {{ formatRate(currentErrors) }}</span></template>
        <Sparkline :first="trend.errors" label="错误趋势" />
      </Card>
    </section>

    <Collapse v-if="snapshot" class="detail-collapse">
      <CollapseItem key="details" header="运行详情">
        <div class="detail-grid">
          <div><span>Key fingerprint</span><code>{{ snapshot.transport.key_fingerprint || "未配置" }}</code></div>
          <div><span>Management auth failures</span><strong>{{ snapshot.metrics.management_auth_failures_total }}</strong></div>
          <div><span>Rate limited</span><strong>{{ snapshot.metrics.management_auth_rate_limited_total }}</strong></div>
          <div><span>GSO</span><strong>{{ snapshot.metrics.quic.gso ? "enabled" : "disabled" }}</strong></div>
          <div><span>Events in view</span><strong>{{ events.length }}</strong></div>
          <div><span>Generated at</span><strong>{{ new Date(snapshot.generated_at).toLocaleTimeString() }}</strong></div>
        </div>
      </CollapseItem>
    </Collapse>
  </div>
</template>
