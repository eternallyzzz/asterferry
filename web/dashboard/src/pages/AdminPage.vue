<script setup lang="ts">
import { ref } from "vue";
import PageHeader from "../components/ui/PageHeader.vue";
import PanelCard from "../components/ui/PanelCard.vue";
import DataTable from "../components/ui/DataTable.vue";
import StatusPill from "../components/ui/StatusPill.vue";
import ModalDialog from "../components/ui/ModalDialog.vue";
import FormField from "../components/ui/FormField.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Spinner from "../components/ui/Spinner.vue";
import UserTokenDialog from "../components/admin/UserTokenDialog.vue";
import {
  createEnrollmentToken,
  createUser,
  deleteUser,
  listEnrollmentTokens,
  listUsers,
  revokeEnrollmentToken,
  updateUser,
  getRuntimeSettings,
  setRuntimeSettings,
  type ControllerUser,
  type EnrollmentTokenMeta,
} from "../controller-api";
import { usePolling } from "../composables/usePolling";
import { useNotify } from "../composables/useNotify";
import { copyText, describeError, formatTime, newIdempotencyKey, userRoleLabel } from "../utils/format";

const notify = useNotify();
const loading = ref(true);
const users = ref<ControllerUser[]>([]);
const tokens = ref<EnrollmentTokenMeta[]>([]);
const advancedOperationsEnabled = ref(false);
const savingRuntimeSettings = ref(false);

const userFormOpen = ref(false);
const userForm = ref({ username: "", password: "", role: "viewer" as ControllerUser["role"] });
const userFormError = ref("");
const savingUser = ref(false);
const pendingDeleteUser = ref<ControllerUser | null>(null);
const deletingUser = ref(false);
const tokenDialogUser = ref<ControllerUser | null>(null);

const enrollTTL = ref("900");
const issuing = ref(false);
const issuedToken = ref("");
const pendingRevoke = ref<EnrollmentTokenMeta | null>(null);
const revoking = ref(false);

async function load() {
  try {
    const [userResult, tokenResult, runtimeSettings] = await Promise.all([listUsers(), listEnrollmentTokens(), getRuntimeSettings()]);
    users.value = userResult.items;
    tokens.value = tokenResult.items;
    advancedOperationsEnabled.value = runtimeSettings.advanced_operations_enabled;
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
}

const { refresh } = usePolling(load);

async function toggleAdvancedOperations() {
  savingRuntimeSettings.value = true;
  const next = !advancedOperationsEnabled.value;
  try {
    const result = await setRuntimeSettings(next, undefined, newIdempotencyKey());
    advancedOperationsEnabled.value = result.advanced_operations_enabled;
    notify.success(next ? "高级运行时操作已开启。" : "高级运行时操作已关闭，在线节点的限速策略已请求清除。" );
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    savingRuntimeSettings.value = false;
  }
}

function isExpired(item: EnrollmentTokenMeta): boolean {
  return new Date(item.expires_at).getTime() < Date.now();
}

async function saveUser() {
  savingUser.value = true;
  userFormError.value = "";
  try {
    await createUser(userForm.value, undefined, newIdempotencyKey());
    notify.success(`用户 ${userForm.value.username} 已创建。`);
    userFormOpen.value = false;
    userForm.value = { username: "", password: "", role: "viewer" };
    await refresh();
  } catch (caught) {
    userFormError.value = describeError(caught);
  } finally {
    savingUser.value = false;
  }
}

async function toggleUser(user: ControllerUser) {
  try {
    await updateUser(user.id, { enabled: !user.enabled }, user.revision, undefined, newIdempotencyKey());
    notify.success(`用户 ${user.username} 已${user.enabled ? "禁用" : "启用"}。`);
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  }
}

async function confirmDeleteUser() {
  if (!pendingDeleteUser.value) return;
  deletingUser.value = true;
  try {
    await deleteUser(pendingDeleteUser.value.id, pendingDeleteUser.value.revision, undefined, newIdempotencyKey());
    notify.success(`用户 ${pendingDeleteUser.value.username} 已删除。`);
    pendingDeleteUser.value = null;
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    deletingUser.value = false;
  }
}

async function issueToken() {
  issuing.value = true;
  try {
    const ttl = Math.max(60, Math.min(900, Number(enrollTTL.value) || 900));
    const result = await createEnrollmentToken(ttl, undefined, newIdempotencyKey());
    issuedToken.value = result.token;
    notify.success("令牌已创建；明文只显示在当前页面，请立即复制。");
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    issuing.value = false;
  }
}

async function copyIssuedToken() {
  const ok = await copyText(issuedToken.value);
  notify[ok ? "success" : "error"](ok ? "token 已复制到剪贴板。" : "浏览器拒绝访问剪贴板，请手动复制。");
}

async function confirmRevoke() {
  if (!pendingRevoke.value) return;
  revoking.value = true;
  try {
    await revokeEnrollmentToken(pendingRevoke.value.id, undefined, newIdempotencyKey());
    notify.success(`token ${pendingRevoke.value.id} 已吊销。`);
    pendingRevoke.value = null;
    await refresh();
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    revoking.value = false;
  }
}
</script>

<template>
  <div class="page-stack">
    <PageHeader
      eyebrow="Identity & Enrollment"
      title="管理"
      description="用户、API token 和节点 enrollment token 只由 Admin 管理。"
    >
      <template #actions>
        <button type="button" class="af-button primary" @click="userFormOpen = true">创建用户</button>
      </template>
    </PageHeader>

    <PanelCard :title="`用户 · ${users.length}`">
      <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
      <DataTable v-else :empty="!users.length">
        <thead>
          <tr><th>用户名</th><th>角色</th><th>状态</th><th>Revision</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="user in users" :key="user.id">
            <td><strong>{{ user.username }}</strong></td>
            <td>{{ userRoleLabel(user.role) }}</td>
            <td><StatusPill :tone="user.enabled ? 'good' : 'neutral'">{{ user.enabled ? "启用" : "停用" }}</StatusPill></td>
            <td>{{ user.revision }}</td>
            <td>
              <div class="row-actions">
                <button type="button" class="af-button text" @click="tokenDialogUser = user">API token</button>
                <button type="button" class="af-button text" @click="toggleUser(user)">{{ user.enabled ? "禁用" : "启用" }}</button>
                <button type="button" class="af-button danger-text" @click="pendingDeleteUser = user">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
        <template #empty>
          <EmptyState title="暂无用户" />
        </template>
      </DataTable>
    </PanelCard>

    <PanelCard title="高级运行时运维">
      <div class="runtime-setting">
        <div>
          <strong>连接级操作</strong>
          <p class="form-note">开启后，Operator 才能在节点详情中断开单条连接、按来源/业务选择连接并动态限速。基础连接 IP、协议、带宽与状态展示始终可见；不采集业务载荷。</p>
        </div>
        <button type="button" :class="['af-button', advancedOperationsEnabled ? 'danger' : 'primary']" :disabled="savingRuntimeSettings" @click="toggleAdvancedOperations">{{ savingRuntimeSettings ? "保存中…" : advancedOperationsEnabled ? "关闭高级操作" : "开启高级操作" }}</button>
      </div>
      <p class="form-note">运行时事件与分钟流量汇总默认保留 30 天。</p>
    </PanelCard>

    <div class="enroll-grid">
      <PanelCard title="签发 enrollment token">
        <form class="enroll-form" @submit.prevent="issueToken">
          <p class="form-note">这里签发的是通用 Node token。节点注册成功后，再在节点详情中保存 Gateway 或 Agent 行为规格。</p>
          <FormField label="有效期（秒）" hint="60 - 900">
            <input v-model="enrollTTL" type="number" min="60" max="900" />
          </FormField>
          <button type="submit" class="af-button primary" :disabled="issuing">{{ issuing ? "签发中…" : "生成一次性 token" }}</button>
        </form>
        <div v-if="issuedToken" class="secret-output">
          <span class="secret-label">仅显示一次</span>
          <code class="mono">{{ issuedToken }}</code>
          <button type="button" class="af-button text" @click="copyIssuedToken">复制</button>
        </div>
      </PanelCard>

      <PanelCard :title="`Enrollment token 历史 · ${tokens.length}`">
        <template #actions>
          <span>哈希不会返回</span>
        </template>
        <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
        <DataTable v-else :empty="!tokens.length">
          <thead>
            <tr><th>ID</th><th>类型</th><th>过期时间</th><th>状态</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="item in tokens" :key="item.id">
              <td><code>{{ item.id }}</code></td>
              <td>通用 Node</td>
              <td>{{ formatTime(item.expires_at) }}</td>
              <td>
                <StatusPill v-if="item.used_at" tone="neutral">已使用/吊销</StatusPill>
                <StatusPill v-else-if="isExpired(item)" tone="warn">已过期</StatusPill>
                <StatusPill v-else tone="good">有效</StatusPill>
              </td>
              <td>
                <button v-if="!item.used_at" type="button" class="af-button danger-text" @click="pendingRevoke = item">吊销</button>
              </td>
            </tr>
          </tbody>
          <template #empty>
            <EmptyState title="暂无 token 记录" description="签发的 token 会按节点行为与过期时间记录在这里。" />
          </template>
        </DataTable>
      </PanelCard>
    </div>

    <ModalDialog :open="userFormOpen" title="创建 Controller 用户" width="440px" @close="userFormOpen = false">
      <form class="form-stack" @submit.prevent="saveUser">
        <FormField label="用户名">
          <input v-model="userForm.username" required autocomplete="off" />
        </FormField>
        <FormField label="初始密码">
          <input v-model="userForm.password" required type="password" autocomplete="new-password" />
        </FormField>
        <FormField label="角色">
          <select v-model="userForm.role">
            <option value="viewer">Viewer · 只读</option>
            <option value="operator">Operator · 运维</option>
            <option value="admin">Admin · 管理</option>
          </select>
        </FormField>
        <p v-if="userFormError" class="form-error">{{ userFormError }}</p>
      </form>
      <template #footer>
        <button type="button" class="af-button secondary" @click="userFormOpen = false">取消</button>
        <button type="button" class="af-button primary" :disabled="savingUser" @click="saveUser">{{ savingUser ? "创建中…" : "创建用户" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingDeleteUser)" title="删除用户" @close="pendingDeleteUser = null">
      <p class="confirm-text">确定删除用户 <strong>{{ pendingDeleteUser?.username }}</strong> 吗？其 API token 会一并失效。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingDeleteUser = null">取消</button>
        <button type="button" class="af-button danger" :disabled="deletingUser" @click="confirmDeleteUser">{{ deletingUser ? "删除中…" : "确认删除" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingRevoke)" title="吊销 enrollment token" @close="pendingRevoke = null">
      <p class="confirm-text">确定吊销 token <strong>{{ pendingRevoke?.id }}</strong> 吗？未使用的令牌将立即失效。</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingRevoke = null">取消</button>
        <button type="button" class="af-button danger" :disabled="revoking" @click="confirmRevoke">{{ revoking ? "吊销中…" : "确认吊销" }}</button>
      </template>
    </ModalDialog>

    <UserTokenDialog :user="tokenDialogUser" @close="tokenDialogUser = null" />
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
  justify-content: center;
  align-items: center;
  min-height: 160px;
}
.row-actions {
  display: flex;
  align-items: center;
  gap: 2px;
}
.enroll-grid {
  display: grid;
  grid-template-columns: minmax(300px, 0.8fr) minmax(0, 1.2fr);
  gap: 16px;
  align-items: start;
}
.enroll-form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.secret-output {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 14px;
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
.form-stack {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.form-error {
  margin: 0;
  color: var(--af-red);
  font-size: 12px;
}
.confirm-text {
  margin: 0;
  color: var(--af-muted);
  line-height: 1.7;
}
.runtime-setting {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
}
@media (max-width: 700px) {
  .runtime-setting { align-items: flex-start; flex-direction: column; }
}
@media (max-width: 900px) {
  .enroll-grid { grid-template-columns: 1fr; }
}
</style>
