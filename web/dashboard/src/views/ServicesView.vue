<script setup lang="ts">
import { computed } from "vue";
import { Alert, Button, Card, Empty, Space, Tag } from "@arco-design/web-vue";
import { useRouter } from "vue-router";
import { useDashboardContext } from "../dashboard-context";
import { mappingStateLabel } from "../model";
import TermHelp from "../components/TermHelp.vue";

const { snapshot } = useDashboardContext();
const router = useRouter();
const role = computed(() => snapshot.value?.role || "agent");
const gatewayMappings = computed(() => snapshot.value?.gateway?.mappings || []);
const reverseMappings = computed(() => snapshot.value?.agent?.reverse_mappings || []);

function open(path: string) {
  void router.push(path);
}

function isLoopback(value: string) {
  return value === "127.0.0.1" || value === "localhost" || value === "::1";
}
</script>

<template>
  <div class="page-stack">
    <div class="page-heading">
      <div>
        <p class="eyebrow">SERVICE INVENTORY</p>
        <h2>内网服务</h2>
        <p class="muted">查看当前 reverse mapping 的状态、监听范围和连接归属。</p>
      </div>
      <Button type="outline" @click="open('/help#operations')">查看操作指南</Button>
    </div>

    <Alert type="info" show-icon>
      当前版本网页只读展示 reverse mapping。新增或修改服务请编辑 Agent 配置中的 <code>agent.reverse</code>，再使用 CLI 或设置页 Apply。
    </Alert>

    <Card v-if="role === 'gateway'" :bordered="false" class="table-card">
      <template #title>Gateway mappings <TermHelp term="Reverse mapping" description="把 Agent 内网服务接入 Gateway 端口的配置。" target="concepts" /></template>
      <template #extra><Tag color="arcoblue">{{ gatewayMappings.length }} 个</Tag></template>
      <div v-if="gatewayMappings.length" class="data-table-wrap">
        <table class="data-table"><thead><tr><th>名称</th><th>Agent</th><th>协议</th><th>监听</th><th>状态</th></tr></thead><tbody>
          <tr v-for="mapping in gatewayMappings" :key="mapping.agent_id + '/' + mapping.name">
            <td><strong>{{ mapping.name }}</strong></td>
            <td><code>{{ mapping.agent_id }}</code></td>
            <td>{{ mapping.protocol.toUpperCase() }}</td>
            <td><code>{{ mapping.gateway_bind }}:{{ mapping.gateway_port }}</code></td>
            <td><Tag :color="mapping.state === 'active' ? 'green' : 'orange'">{{ mappingStateLabel(mapping.state) }}</Tag></td>
          </tr>
        </tbody></table>
      </div>
      <Empty v-else description="暂无 reverse mapping" />
    </Card>

    <Card v-else :bordered="false" class="table-card">
      <template #title>Agent reverse mappings <TermHelp term="Reverse mapping" description="把本机内网服务映射到 Gateway 端口的配置。" target="concepts" /></template>
      <template #extra><Tag :color="snapshot?.agent?.connected ? 'green' : 'orange'">{{ snapshot?.agent?.connected ? 'Agent 已连接' : 'Agent 离线' }}</Tag></template>
      <div v-if="reverseMappings.length" class="data-table-wrap">
        <table class="data-table"><thead><tr><th>名称</th><th>本地地址</th><th>协议</th><th>Gateway 监听</th><th>暴露范围</th></tr></thead><tbody>
          <tr v-for="mapping in reverseMappings" :key="mapping.name">
            <td><strong>{{ mapping.name }}</strong></td>
            <td><code>{{ mapping.local }}</code></td>
            <td>{{ mapping.protocol.toUpperCase() }}</td>
            <td><code>{{ mapping.gateway_bind }}:{{ mapping.gateway_port }}</code></td>
            <td><Tag :color="isLoopback(mapping.gateway_bind) ? 'orange' : 'green'">{{ isLoopback(mapping.gateway_bind) ? '仅本机' : '可被外部访问' }}</Tag></td>
          </tr>
        </tbody></table>
      </div>
      <Empty v-else description="还没有配置 reverse mapping">
        <template #extra><Button type="primary" @click="open('/settings')">打开配置</Button></template>
      </Empty>
    </Card>

    <Card :bordered="false" class="next-step-card">
      <Space>
        <span class="next-step-icon">→</span>
        <div><strong>下一步</strong><p class="muted">想了解 Gateway、Agent 和 Reverse mapping 的关系？</p></div>
        <Button type="text" @click="open('/help#concepts')">查看概念速查</Button>
      </Space>
    </Card>
  </div>
</template>
