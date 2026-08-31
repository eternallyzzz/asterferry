<script setup lang="ts">
import { ref, watch } from "vue";
import ModalDialog from "../ui/ModalDialog.vue";
import DataTable from "../ui/DataTable.vue";
import StatusPill from "../ui/StatusPill.vue";
import FormField from "../ui/FormField.vue";
import EmptyState from "../ui/EmptyState.vue";
import Spinner from "../ui/Spinner.vue";
import {
  createUserToken,
  listUserTokens,
  revokeUserToken,
  type APITokenMeta,
  type ControllerUser,
} from "../../controller-api";
import { useNotify } from "../../composables/useNotify";
import { copyText, describeError, formatTime, newIdempotencyKey } from "../../utils/format";

const props = defineProps<{ user: ControllerUser | null }>();
const emit = defineEmits<{ close: [] }>();

const notify = useNotify();
const loading = ref(false);
const tokens = ref<APITokenMeta[]>([]);
const name = ref("");
const expiresAt = ref("");
const creating = ref(false);
const createError = ref("");
const issuedToken = ref("");
const pendingRevoke = ref<APITokenMeta | null>(null);
const revoking = ref(false);

async function load() {
  if (!props.user) return;
  loading.value = true;
  try {
    const result = await listUserTokens(props.user.id);
    tokens.value = result.items;
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
}

watch(
  () => props.user?.id,
  (id) => {
    issuedToken.value = "";
    createError.value = "";
    name.value = "";
    expiresAt.value = "";
    tokens.value = [];
    if (id) void load();
  },
);

function isExpired(item: APITokenMeta): boolean {
  return Boolean(item.expires_at) && new Date(item.expires_at as string).getTime() < Date.now();
}

async function create() {
  if (!props.user) return;
  creating.value = true;
  createError.value = "";
  try {
    const expiry = expiresAt.value ? new Date(expiresAt.value).toISOString() : undefined;
    const result = await createUserToken(props.user.id, name.value.trim(), expiry, undefined, newIdempotencyKey());
    issuedToken.value = result.token;
    name.value = "";
    expiresAt.value = "";
    notify.success("API token 已创建；明文只显示一次，请立即复制。");
    await load();
  } catch (caught) {
    createError.value = describeError(caught);
  } finally {
    creating.value = false;
  }
}

async function copyIssued() {
  const ok = await copyText(issuedToken.value);
  notify[ok ? "success" : "error"](ok ? "token 已复制到剪贴板。" : "浏览器拒绝访问剪贴板，请手动复制。");
}

async function confirmRevoke() {
  if (!props.user || !pendingRevoke.value) return;
  revoking.value = true;
  try {
    await revokeUserToken(props.user.id, pendingRevoke.value.id, undefined, newIdempotencyKey());
    notify.success(`token ${pendingRevoke.value.name} 已吊销。`);
    pendingRevoke.value = null;
    await load();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    revoking.value = false;
  }
}
</script>

<template>
  <ModalDialog :open="Boolean(user)" :title="`API token · ${user?.username ?? ''}`" width="640px" @close="emit('close')">
    <div class="token-dialog">
      <form class="create-form" @submit.prevent="create">
        <FormField label="名称" class="grow">
          <input v-model="name" required placeholder="ci-automation" />
        </FormField>
        <FormField label="过期时间（可选）" class="grow">
          <input v-model="expiresAt" type="datetime-local" />
        </FormField>
        <button type="submit" class="af-button primary create-btn" :disabled="creating">{{ creating ? "签发中…" : "签发" }}</button>
      </form>
      <p v-if="createError" class="form-error">{{ createError }}</p>

      <div v-if="issuedToken" class="secret-output">
        <span class="secret-label">仅显示一次</span>
        <code class="mono">{{ issuedToken }}</code>
        <button type="button" class="af-button text" @click="copyIssued">复制</button>
      </div>

      <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
      <DataTable v-else :empty="!tokens.length">
        <thead>
          <tr><th>名称</th><th>过期时间</th><th>创建时间</th><th>状态</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="item in tokens" :key="item.id">
            <td><strong>{{ item.name }}</strong></td>
            <td>{{ item.expires_at ? formatTime(item.expires_at) : "永不过期" }}</td>
            <td>{{ formatTime(item.created_at) }}</td>
            <td>
              <StatusPill v-if="item.revoked_at" tone="neutral">已吊销</StatusPill>
              <StatusPill v-else-if="isExpired(item)" tone="warn">已过期</StatusPill>
              <StatusPill v-else tone="good">有效</StatusPill>
            </td>
            <td>
              <button v-if="!item.revoked_at" type="button" class="af-button danger-text" @click="pendingRevoke = item">吊销</button>
            </td>
          </tr>
        </tbody>
        <template #empty>
          <EmptyState title="暂无 API token" description="签发的 token 用于自动化访问 /api/v1，哈希不会回显。" />
        </template>
      </DataTable>
    </div>

    <ModalDialog :open="Boolean(pendingRevoke)" title="吊销 API token" @close="pendingRevoke = null">
      <p class="confirm-text">确定吊销 token <strong>{{ pendingRevoke?.name }}</strong> 吗？使用它的自动化调用会立即失效。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingRevoke = null">取消</button>
        <button type="button" class="af-button danger" :disabled="revoking" @click="confirmRevoke">{{ revoking ? "吊销中…" : "确认吊销" }}</button>
      </template>
    </ModalDialog>
  </ModalDialog>
</template>

<style scoped>
.token-dialog {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.create-form {
  display: flex;
  align-items: flex-end;
  gap: 10px;
}
.create-form .grow { flex: 1 1 0; }
.create-btn { flex: 0 0 auto; height: 37px; }
.form-error {
  margin: 0;
  color: var(--af-red);
  font-size: 12px;
}
.secret-output {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  padding: 10px 12px;
  border: 1px solid var(--af-amber-soft);
  border-radius: var(--af-radius-sm);
  background: var(--af-amber-soft);
}
.secret-label {
  flex: 0 0 auto;
  color: var(--af-amber);
  font-size: 11px;
  font-weight: 600;
}
.secret-output code {
  flex: 1 1 200px;
  color: var(--af-text);
  font-size: 12px;
  overflow-wrap: anywhere;
}
.loading-row {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 120px;
}
.confirm-text {
  margin: 0;
  color: var(--af-muted);
  line-height: 1.7;
}
@media (max-width: 640px) {
  .create-form { flex-direction: column; align-items: stretch; }
}
</style>
