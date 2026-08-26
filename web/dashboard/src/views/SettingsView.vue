<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { Alert, Button, Card, Tag } from "@arco-design/web-vue";
import {
  APIError,
  applyConfig,
  fetchConfig,
  rollbackConfig,
  validateConfig,
  type ConfigObject,
  type ConfigPayload,
  type ConfigSnapshot,
  type ConfigValidation,
} from "../api";
import { cloneObject, renderYAML } from "../config-utils";
import { useSession, viewerTokenErrorMessage } from "../session";
import AdminTokenModal from "../components/AdminTokenModal.vue";
import ConfigForm from "../components/ConfigForm.vue";

type EditorMode = "form" | "yaml";
type PendingAction = "apply" | "rollback";

const session = useSession();
const snapshot = ref<ConfigSnapshot | null>(null);
const values = ref<ConfigObject | null>(null);
const yaml = ref("");
const mode = ref<EditorMode>("form");
const validation = ref<ConfigValidation | null>(null);
const loading = ref(false);
const actionBusy = ref(false);
const error = ref("");
const message = ref("");
const adminVisible = ref(false);
const adminError = ref("");
const pendingAction = ref<PendingAction | null>(null);

const payload = computed<ConfigPayload | null>(() => {
  if (!snapshot.value || !values.value) return null;
  return mode.value === "yaml"
    ? { base_revision: snapshot.value.revision, yaml: yaml.value }
    : { base_revision: snapshot.value.revision, config: values.value };
});

async function load() {
  loading.value = true;
  error.value = "";
  try {
    const next = await fetchConfig(session.viewerToken.value);
    snapshot.value = next;
    values.value = cloneObject(next.values);
    yaml.value = next.yaml;
    mode.value = "form";
    validation.value = null;
    message.value = "";
  } catch (caught) {
    if (caught instanceof APIError && caught.status === 401) {
      session.invalidateViewer(viewerTokenErrorMessage);
      return;
    }
    error.value = caught instanceof Error ? caught.message : "配置加载失败。";
  } finally {
    loading.value = false;
  }
}

onMounted(() => void load());

async function validateCurrent(): Promise<ConfigValidation | null> {
  if (!payload.value) return null;
  loading.value = true;
  error.value = "";
  message.value = "";
  try {
    const result = await validateConfig(session.viewerToken.value, payload.value);
    validation.value = result;
    message.value = result.changed ? "配置有效，请检查 diff 后再 Apply。" : "没有检测到配置变化。";
    return result;
  } catch (caught) {
    if (caught instanceof APIError && caught.status === 401) {
      session.invalidateViewer(viewerTokenErrorMessage);
      return null;
    }
    validation.value = null;
    error.value = caught instanceof Error ? caught.message : "配置校验失败。";
    return null;
  } finally {
    loading.value = false;
  }
}

function switchMode(next: EditorMode) {
  if (next === mode.value) return;
  if (next === "form" && snapshot.value && yaml.value !== snapshot.value.yaml && !window.confirm("切换结构化表单会丢弃未保存的 YAML 修改，继续吗？")) return;
  if (next === "yaml" && values.value) yaml.value = renderYAML(values.value);
  mode.value = next;
  validation.value = null;
  message.value = "";
}

async function startApply() {
  if (!snapshot.value?.writable || !payload.value) return;
  if (!validation.value?.changed) {
    const result = await validateCurrent();
    if (!result?.changed) return;
  }
  requestAdmin("apply");
}

function startRollback() {
  if (snapshot.value?.backup_available) requestAdmin("rollback");
}

function requestAdmin(action: PendingAction) {
  pendingAction.value = action;
  adminError.value = "";
  if (session.adminToken.value) {
    void performAdminAction(action);
    return;
  }
  adminVisible.value = true;
}

async function confirmAdmin(token: string) {
  session.setAdminToken(token);
  adminVisible.value = false;
  if (pendingAction.value) await performAdminAction(pendingAction.value);
}

async function performAdminAction(action: PendingAction) {
  if (!snapshot.value || !session.adminToken.value) return;
  actionBusy.value = true;
  error.value = "";
  adminError.value = "";
  try {
    if (action === "apply" && payload.value) {
      const result = await applyConfig(session.adminToken.value, payload.value);
      message.value = `配置已写入，正在请求 ${result.role} restart。页面会自动重新连接。`;
    } else if (action === "rollback") {
      const result = await rollbackConfig(session.adminToken.value, snapshot.value.revision);
      message.value = `已请求恢复上一份配置（${result.role}），页面会自动重新连接。`;
    }
    validation.value = null;
    window.setTimeout(() => void load(), 1500);
  } catch (caught) {
    if (caught instanceof APIError && (caught.status === 401 || caught.status === 403)) {
      session.clearAdmin();
      adminError.value = caught.message || "Admin token 无效或权限不足。";
      adminVisible.value = true;
      return;
    }
    error.value = caught instanceof Error ? caught.message : "配置写入失败。";
  } finally {
    actionBusy.value = false;
  }
}
</script>

<template>
  <div class="page-stack">
    <div class="page-heading">
      <div><p class="eyebrow">ADVANCED SETTINGS</p><h2>设置</h2><p class="muted">基础配置保持简单，高级项默认收起。Secret 字段仍然只读。</p></div>
      <Button type="outline" :loading="loading" @click="load">重新加载</Button>
    </div>

    <Alert v-if="error" type="error" show-icon>{{ error }}</Alert>
    <Alert v-if="message" type="success" show-icon>{{ message }}</Alert>
    <Alert type="info" show-icon>
      Validate 和 preview 使用 Viewer token；Apply、Rollback 会在点击后要求 Admin token。Inbound password 等 secret 只能直接编辑配置文件。
    </Alert>

    <template v-if="snapshot && values">
      <div class="config-meta">
        <Tag :color="snapshot.writable ? 'green' : 'orange'">{{ snapshot.writable ? '可写配置' : '只读配置' }}</Tag>
        <span>{{ snapshot.backup_available ? '已有上一份备份' : '暂无备份' }}</span>
        <code>{{ snapshot.revision.slice(0, 16) }}…</code>
      </div>
      <div class="editor-tabs">
        <Button :type="mode === 'form' ? 'primary' : 'outline'" @click="switchMode('form')">结构化表单</Button>
        <Button :type="mode === 'yaml' ? 'primary' : 'outline'" @click="switchMode('yaml')">Advanced YAML</Button>
      </div>
      <ConfigForm v-if="mode === 'form'" :values="values" :role="snapshot.role" @update="values = $event" />
      <Card v-else :bordered="false" class="settings-card">
        <textarea v-model="yaml" class="config-editor" spellcheck="false" @input="validation = null" />
      </Card>
      <Card v-if="validation" :bordered="false" class="validation-card">
        <strong>{{ validation.changed ? '配置有效，待 Apply' : '没有变化' }}</strong>
        <p v-for="warning in validation.warnings" :key="warning" class="warning-line">{{ warning }}</p>
        <pre v-if="validation.diff">{{ validation.diff }}</pre>
      </Card>
      <div class="config-actions">
        <Button @click="validateCurrent" :loading="loading">校验并预览</Button>
        <Button type="primary" :disabled="!snapshot.writable || !validation?.changed" :loading="actionBusy" @click="startApply">Apply 配置</Button>
        <Button v-if="snapshot.backup_available" status="warning" :loading="actionBusy" @click="startRollback">Rollback</Button>
      </div>
    </template>
    <Card v-else-if="!loading" :bordered="false"><p class="empty">配置暂不可用。</p></Card>
    <div v-else class="loading-card"><span class="spinner" /> 正在加载配置…</div>

    <AdminTokenModal v-model:visible="adminVisible" :busy="actionBusy" :error="adminError" @confirm="confirmAdmin" />
  </div>
</template>
