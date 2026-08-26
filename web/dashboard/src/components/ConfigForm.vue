<script setup lang="ts">
import { computed } from "vue";
import { Card, Form, FormItem, Input, InputNumber, Switch, Typography } from "@arco-design/web-vue";
import type { ConfigObject, ConfigValue, Role } from "../api";
import { getPath, setPath } from "../config-utils";

const props = defineProps<{ values: ConfigObject; role: Role }>();
const emit = defineEmits<{ update: [value: ConfigObject] }>();

function read(path: string, fallback: string | number | boolean): string | number | boolean {
  const value = getPath(props.values, path);
  return typeof value === "string" || typeof value === "number" || typeof value === "boolean" ? value : fallback;
}

function text(path: string, fallback: string) {
  return String(read(path, fallback));
}

function number(path: string, fallback: number) {
  return Number(read(path, fallback));
}

function boolean(path: string, fallback: boolean) {
  return Boolean(read(path, fallback));
}

function update(path: string, value: string | number | boolean) {
  emit("update", setPath(props.values, path, value));
}

const roleTitle = computed(() => props.role === "gateway" ? "Gateway" : "Agent");
</script>

<template>
  <div class="config-form-grid">
    <Card title="Management web" :bordered="false" class="settings-card">
      <Form :model="values" layout="vertical">
        <FormItem label="Management listen"><Input :model-value="text('management.listen', '127.0.0.1:9090')" @update:model-value="update('management.listen', $event)" /></FormItem>
        <FormItem label="Embedded Dashboard"><Switch :model-value="boolean('management.web.enabled', true)" @update:model-value="update('management.web.enabled', $event)" /></FormItem>
        <FormItem label="TLS certificate file"><Input :model-value="text('management.tls.cert_file', '')" @update:model-value="update('management.tls.cert_file', $event)" /></FormItem>
        <FormItem label="TLS key file"><Input :model-value="text('management.tls.key_file', '')" @update:model-value="update('management.tls.key_file', $event)" /></FormItem>
      </Form>
    </Card>
    <Card title="Runtime" :bordered="false" class="settings-card">
      <Form :model="values" layout="vertical">
        <FormItem label="Logging level"><Input :model-value="text('logging.level', 'info')" @update:model-value="update('logging.level', $event)" /></FormItem>
        <FormItem label="Logging format"><Input :model-value="text('logging.format', 'json')" @update:model-value="update('logging.format', $event)" /></FormItem>
        <FormItem label="Shutdown grace period (seconds)"><InputNumber :model-value="number('shutdown.grace_period_seconds', 30)" :min="1" @update:model-value="update('shutdown.grace_period_seconds', $event || 0)" /></FormItem>
        <FormItem label="Transport ALPN"><Input :model-value="text('transport.alpn', '')" @update:model-value="update('transport.alpn', $event)" /></FormItem>
        <FormItem label="Handshake timeout (seconds)"><InputNumber :model-value="number('transport.handshake_timeout_seconds', 10)" :min="1" @update:model-value="update('transport.handshake_timeout_seconds', $event || 0)" /></FormItem>
      </Form>
    </Card>
    <Card :title="roleTitle" :bordered="false" class="settings-card">
      <Form :model="values" layout="vertical">
        <template v-if="role === 'gateway'">
          <FormItem label="Gateway listen"><Input :model-value="text('gateway.listen', '')" @update:model-value="update('gateway.listen', $event)" /></FormItem>
        </template>
        <template v-else>
          <FormItem label="Gateway server"><Input :model-value="text('agent.server', '')" @update:model-value="update('agent.server', $event)" /></FormItem>
          <FormItem label="Default route"><Input :model-value="text('agent.proxy.default_route', 'gateway')" @update:model-value="update('agent.proxy.default_route', $event)" /></FormItem>
        </template>
      </Form>
      <Typography.Paragraph class="muted config-hint">
        Agent 列表、reverse mappings、ACL、路由规则等完整配置请切换到 Advanced YAML。
      </Typography.Paragraph>
    </Card>
  </div>
</template>
