<script setup lang="ts">
import { computed } from "vue";
import { Card, Empty, Tag } from "@arco-design/web-vue";
import { useDashboardContext } from "../dashboard-context";

const { snapshot } = useDashboardContext();
const gatewayAgents = computed(() => snapshot.value?.gateway?.agents || []);
</script>

<template>
  <div class="page-stack">
    <div class="page-heading">
      <div><p class="eyebrow">RUNTIME NODES</p><h2>节点</h2><p class="muted">回答“连上了吗”，显示当前管理端能观察到的 Gateway / Agent 会话。</p></div>
    </div>
    <Card :bordered="false" class="table-card">
      <template #title>{{ snapshot?.role === 'gateway' ? 'Gateway 与 Agent 会话' : '本 Agent 会话' }}</template>
      <template #extra><Tag color="arcoblue">{{ snapshot?.role === 'gateway' ? gatewayAgents.length + 1 : 2 }} 个节点</Tag></template>
      <div v-if="snapshot?.role === 'gateway'" class="data-table-wrap">
        <table class="data-table"><thead><tr><th>节点</th><th>Node ID</th><th>Session</th><th>连接状态</th><th>Mappings</th></tr></thead><tbody>
          <tr><td><strong>Gateway</strong></td><td><code>{{ snapshot.node_id || 'local' }}</code></td><td>—</td><td><Tag color="green">在线</Tag></td><td>—</td></tr>
          <tr v-for="agent in gatewayAgents" :key="agent.agent_id"><td><strong>{{ agent.agent_id }}</strong></td><td><code>{{ agent.node_id || '—' }}</code></td><td><code>{{ agent.session_id || '—' }}</code></td><td><Tag :color="agent.connected ? 'green' : 'orange'">{{ agent.connected ? '在线' : '离线' }}</Tag></td><td>{{ agent.mapping_count }}</td></tr>
        </tbody></table>
      </div>
      <div v-else-if="snapshot?.agent" class="data-table-wrap">
        <table class="data-table"><thead><tr><th>节点</th><th>Node ID</th><th>Session</th><th>连接状态</th><th>重连次数</th></tr></thead><tbody>
          <tr><td><strong>Agent · {{ snapshot.agent.agent_id }}</strong></td><td><code>{{ snapshot.node_id || 'local' }}</code></td><td><code>{{ snapshot.agent.session_id || '—' }}</code></td><td><Tag :color="snapshot.agent.connected ? 'green' : 'orange'">{{ snapshot.agent.connected ? '在线' : '离线' }}</Tag></td><td>{{ snapshot.agent.reconnects }}</td></tr>
          <tr><td><strong>Gateway</strong></td><td>—</td><td>—</td><td><Tag :color="snapshot.agent.connected ? 'green' : 'orange'">{{ snapshot.agent.connected ? '已连接' : '离线' }}</Tag></td><td>—</td></tr>
        </tbody></table>
      </div>
      <Empty v-else description="节点数据暂不可用" />
    </Card>
    <Card :bordered="false" class="muted-note">
      当前快照不包含对端 IP 和精确最后心跳时间，因此界面不使用推测值；顶部的实时连接状态和事件流仍可用于判断可用性。
    </Card>
  </div>
</template>
