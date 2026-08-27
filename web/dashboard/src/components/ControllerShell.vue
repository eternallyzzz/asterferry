<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { Alert, Button, Card, Grid, GridItem, Space, Table, Tag } from "@arco-design/web-vue";
import {
  listAssignments,
  listAudit,
  listNodes,
  listServices,
  type ControllerNode,
  type ControllerService,
  type ControllerUser,
} from "../controller-api";

const props = defineProps<{ user: ControllerUser }>();
const emit = defineEmits<{ logout: [] }>();

const loading = ref(false);
const error = ref("");
const nodes = ref<ControllerNode[]>([]);
const services = ref<ControllerService[]>([]);
const assignments = ref<unknown[]>([]);
const audit = ref<unknown[]>([]);
let timer: number | undefined;

const gateways = computed(() => nodes.value.filter((node) => node.role === "gateway"));
const agents = computed(() => nodes.value.filter((node) => node.role === "agent"));
const online = computed(() => nodes.value.filter((node) => node.enabled && node.certificate_state === "active").length);

async function refresh() {
  loading.value = true;
  try {
    const [nodeResult, serviceResult, assignmentResult, auditResult] = await Promise.all([
      listNodes(),
      listServices(),
      listAssignments(),
      listAudit(20),
    ]);
    nodes.value = nodeResult.items;
    services.value = serviceResult.items;
    assignments.value = assignmentResult.items;
    audit.value = auditResult.items;
    error.value = "";
  } catch (caught) {
    error.value = caught instanceof Error ? caught.message : "Controller 数据刷新失败。";
  } finally {
    loading.value = false;
  }
}

onMounted(() => {
  void refresh();
  timer = window.setInterval(() => void refresh(), 5000);
});
onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer);
});
</script>

<template>
  <main class="app-shell controller-shell">
    <header class="app-header">
      <div>
        <div class="section-kicker">ASTERFERRY CONTROLLER</div>
        <h1>控制面</h1>
        <p class="muted">{{ props.user.username }} · {{ props.user.role }}</p>
      </div>
      <Space>
        <Button :loading="loading" @click="refresh">刷新</Button>
        <Button type="outline" @click="emit('logout')">退出</Button>
      </Space>
    </header>

    <Alert v-if="error" type="warning" show-icon class="page-alert">{{ error }}</Alert>

    <Grid :cols="4" :col-gap="16" :row-gap="16" class="metric-grid">
      <GridItem><Card :bordered="false"><div class="mini-stat"><span>节点</span><strong>{{ nodes.length }}</strong></div></Card></GridItem>
      <GridItem><Card :bordered="false"><div class="mini-stat"><span>在线节点</span><strong>{{ online }}</strong></div></Card></GridItem>
      <GridItem><Card :bordered="false"><div class="mini-stat"><span>Gateway / Agent</span><strong>{{ gateways.length }} / {{ agents.length }}</strong></div></Card></GridItem>
      <GridItem><Card :bordered="false"><div class="mini-stat"><span>服务 / Assignment</span><strong>{{ services.length }} / {{ assignments.length }}</strong></div></Card></GridItem>
    </Grid>

    <section class="chart-grid">
      <Card :bordered="false">
        <template #title>节点状态</template>
        <Table :data="nodes" :pagination="false" row-key="id" :bordered="false">
          <template #columns>
            <Table.Column title="名称" data-index="name" />
            <Table.Column title="角色" data-index="role" />
            <Table.Column title="证书">
              <template #cell="{ record }"><Tag :color="record.certificate_state === 'active' ? 'green' : 'orange'">{{ record.certificate_state }}</Tag></template>
            </Table.Column>
            <Table.Column title="Revision" data-index="revision" />
          </template>
        </Table>
      </Card>
      <Card :bordered="false">
        <template #title>内网服务</template>
        <Table :data="services" :pagination="false" row-key="id" :bordered="false">
          <template #columns>
            <Table.Column title="服务" data-index="id" />
            <Table.Column title="协议" data-index="protocol" />
            <Table.Column title="Agent" data-index="agent_id" />
            <Table.Column title="公网端口" data-index="public_port" />
          </template>
        </Table>
      </Card>
    </section>

    <Card :bordered="false">
      <template #title>最近审计</template>
      <pre class="audit-preview">{{ JSON.stringify(audit, null, 2) }}</pre>
    </Card>
  </main>
</template>
