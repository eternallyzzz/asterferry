<script setup lang="ts">
import { computed, ref } from "vue";
import DataTable from "../ui/DataTable.vue";
import StatusPill from "../ui/StatusPill.vue";
import ModalDialog from "../ui/ModalDialog.vue";
import FormField from "../ui/FormField.vue";
import EmptyState from "../ui/EmptyState.vue";
import {
  createNodeProxy,
  createNodeRoute,
  deleteNodeProxy,
  deleteNodeRoute,
  updateNodeProxy,
  updateNodeRoute,
  type ProxySpec,
  type RouteRule,
} from "../../controller-api";
import { useNotify } from "../../composables/useNotify";
import { useSession } from "../../session";
import { describeError, joinList, newIdempotencyKey, splitList } from "../../utils/format";

const props = defineProps<{
  kind: "proxies" | "routes";
  nodeId: string;
  revision: number | undefined;
  items: Array<ProxySpec | RouteRule>;
}>();
const emit = defineEmits<{ changed: [] }>();

const notify = useNotify();
const session = useSession();

const isProxy = computed(() => props.kind === "proxies");
const noun = computed(() => (isProxy.value ? "代理入口" : "路由规则"));

const formOpen = ref(false);
const editing = ref(false);
const saving = ref(false);
const formError = ref("");
const proxyForm = ref({ id: "", protocol: "tcp", bind: "0.0.0.0:1080", route: "", enabled: true });
const routeForm = ref({ name: "", destination: "", cidrs: "", domains: "", geoip: "", enabled: true });
const pendingDelete = ref<ProxySpec | RouteRule | null>(null);
const deleting = ref(false);

function itemKey(item: ProxySpec | RouteRule): string {
  return isProxy.value ? (item as ProxySpec).id : (item as RouteRule).name;
}

function openCreate() {
  editing.value = false;
  formError.value = "";
  proxyForm.value = { id: "", protocol: "tcp", bind: "0.0.0.0:1080", route: "", enabled: true };
  routeForm.value = { name: "", destination: "", cidrs: "", domains: "", geoip: "", enabled: true };
  formOpen.value = true;
}

function openEdit(item: ProxySpec | RouteRule) {
  editing.value = true;
  formError.value = "";
  if (isProxy.value) {
    const proxy = item as ProxySpec;
    proxyForm.value = { id: proxy.id, protocol: proxy.protocol, bind: proxy.bind, route: proxy.route, enabled: proxy.enabled };
  } else {
    const route = item as RouteRule;
    routeForm.value = {
      name: route.name,
      destination: route.destination,
      cidrs: joinList(route.cidrs),
      domains: joinList(route.domains),
      geoip: joinList(route.geoip),
      enabled: route.enabled,
    };
  }
  formOpen.value = true;
}

async function save() {
  if (props.revision === undefined) {
    formError.value = "请先在「规格」分段保存规格文档。";
    return;
  }
  saving.value = true;
  formError.value = "";
  try {
    if (isProxy.value) {
      const proxy: ProxySpec = { id: proxyForm.value.id.trim(), protocol: proxyForm.value.protocol, bind: proxyForm.value.bind.trim(), route: proxyForm.value.route.trim(), enabled: proxyForm.value.enabled };
      if (editing.value) await updateNodeProxy(props.nodeId, proxy.id, proxy, props.revision, undefined, newIdempotencyKey());
      else await createNodeProxy(props.nodeId, proxy, props.revision, undefined, newIdempotencyKey());
    } else {
      const route: RouteRule = {
        name: routeForm.value.name.trim(),
        destination: routeForm.value.destination.trim(),
        cidrs: splitList(routeForm.value.cidrs),
        domains: splitList(routeForm.value.domains),
        geoip: splitList(routeForm.value.geoip),
        enabled: routeForm.value.enabled,
      };
      if (editing.value) await updateNodeRoute(props.nodeId, route.name, route, props.revision, undefined, newIdempotencyKey());
      else await createNodeRoute(props.nodeId, route, props.revision, undefined, newIdempotencyKey());
    }
    notify.success(`${noun.value}已保存。`);
    formOpen.value = false;
    emit("changed");
  } catch (caught) {
    formError.value = describeError(caught);
  } finally {
    saving.value = false;
  }
}

async function confirmDelete() {
  if (!pendingDelete.value || props.revision === undefined) return;
  deleting.value = true;
  try {
    if (isProxy.value) await deleteNodeProxy(props.nodeId, (pendingDelete.value as ProxySpec).id, props.revision, undefined, newIdempotencyKey());
    else await deleteNodeRoute(props.nodeId, (pendingDelete.value as RouteRule).name, props.revision, undefined, newIdempotencyKey());
    notify.success(`${noun.value}已删除。`);
    pendingDelete.value = null;
    emit("changed");
  } catch (caught) {
    notify.error(describeError(caught));
  } finally {
    deleting.value = false;
  }
}
</script>

<template>
  <div class="manager">
    <div class="manager-head">
      <span class="muted head-note">变更以规格 revision {{ revision ?? "new" }} 做 CAS，冲突时刷新后重试。</span>
      <button v-if="session.canOperate.value" type="button" class="af-button secondary" :disabled="revision === undefined" @click="openCreate">添加{{ noun }}</button>
    </div>

    <DataTable :empty="!items.length">
      <thead v-if="isProxy">
        <tr><th>ID</th><th>协议</th><th>监听</th><th>路由</th><th>状态</th><th v-if="session.canOperate.value">操作</th></tr>
      </thead>
      <thead v-else>
        <tr><th>名称</th><th>目的地</th><th>匹配</th><th>状态</th><th v-if="session.canOperate.value">操作</th></tr>
      </thead>
      <tbody v-if="isProxy">
        <tr v-for="item in items" :key="itemKey(item)">
          <td><strong>{{ (item as ProxySpec).id }}</strong></td>
          <td>{{ (item as ProxySpec).protocol.toUpperCase() }}</td>
          <td><code>{{ (item as ProxySpec).bind }}</code></td>
          <td><code>{{ (item as ProxySpec).route || "—" }}</code></td>
          <td><StatusPill :tone="item.enabled ? 'good' : 'neutral'">{{ item.enabled ? "启用" : "停用" }}</StatusPill></td>
          <td v-if="session.canOperate.value">
            <div class="row-actions">
              <button type="button" class="af-button text" @click="openEdit(item)">编辑</button>
              <button type="button" class="af-button danger-text" @click="pendingDelete = item">删除</button>
            </div>
          </td>
        </tr>
      </tbody>
      <tbody v-else>
        <tr v-for="item in items" :key="itemKey(item)">
          <td><strong>{{ (item as RouteRule).name }}</strong></td>
          <td><code>{{ (item as RouteRule).destination }}</code></td>
          <td>
            <small class="match-cell">
              <template v-if="(item as RouteRule).cidrs?.length">CIDR: {{ (item as RouteRule).cidrs?.join(", ") }}<br /></template>
              <template v-if="(item as RouteRule).domains?.length">域名: {{ (item as RouteRule).domains?.join(", ") }}<br /></template>
              <template v-if="(item as RouteRule).geoip?.length">GeoIP: {{ (item as RouteRule).geoip?.join(", ") }}</template>
              <template v-if="!(item as RouteRule).cidrs?.length && !(item as RouteRule).domains?.length && !(item as RouteRule).geoip?.length">—</template>
            </small>
          </td>
          <td><StatusPill :tone="item.enabled ? 'good' : 'neutral'">{{ item.enabled ? "启用" : "停用" }}</StatusPill></td>
          <td v-if="session.canOperate.value">
            <div class="row-actions">
              <button type="button" class="af-button text" @click="openEdit(item)">编辑</button>
              <button type="button" class="af-button danger-text" @click="pendingDelete = item">删除</button>
            </div>
          </td>
        </tr>
      </tbody>
      <template #empty>
        <EmptyState :title="`暂无${noun}`" :description="isProxy ? '代理入口定义 Agent 本地暴露的协议入口。' : '路由规则按 CIDR、域名或 GeoIP 匹配流量并指向目的地。'" />
      </template>
    </DataTable>

    <ModalDialog :open="formOpen" :title="`${editing ? '编辑' : '添加'}${noun}`" width="480px" @close="formOpen = false">
      <form v-if="isProxy" class="form-stack" @submit.prevent="save">
        <FormField label="ID">
          <input v-model="proxyForm.id" :disabled="editing" required placeholder="socks5-in" />
        </FormField>
        <FormField label="协议">
          <select v-model="proxyForm.protocol">
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
          </select>
        </FormField>
        <FormField label="监听地址">
          <input v-model="proxyForm.bind" required placeholder="127.0.0.1:1080" />
        </FormField>
        <FormField label="关联路由" hint="路由规则名称；留空使用默认转发">
          <input v-model="proxyForm.route" placeholder="default" />
        </FormField>
        <label class="check-label"><input v-model="proxyForm.enabled" type="checkbox" /> 启用</label>
        <p v-if="formError" class="form-error">{{ formError }}</p>
      </form>
      <form v-else class="form-stack" @submit.prevent="save">
        <FormField label="名称">
          <input v-model="routeForm.name" :disabled="editing" required placeholder="office-only" />
        </FormField>
        <FormField label="目的地" hint="转发目的地，如 direct 或上游标识">
          <input v-model="routeForm.destination" required placeholder="direct" />
        </FormField>
        <FormField label="CIDR 匹配" hint="逗号分隔，可留空">
          <input v-model="routeForm.cidrs" spellcheck="false" placeholder="10.0.0.0/8" />
        </FormField>
        <FormField label="域名匹配" hint="逗号分隔，可留空">
          <input v-model="routeForm.domains" spellcheck="false" placeholder="example.com, *.internal" />
        </FormField>
        <FormField label="GeoIP 匹配" hint="逗号分隔，可留空">
          <input v-model="routeForm.geoip" spellcheck="false" placeholder="CN" />
        </FormField>
        <label class="check-label"><input v-model="routeForm.enabled" type="checkbox" /> 启用</label>
        <p v-if="formError" class="form-error">{{ formError }}</p>
      </form>
      <template #footer>
        <button type="button" class="af-button secondary" @click="formOpen = false">取消</button>
        <button type="button" class="af-button primary" :disabled="saving" @click="save">{{ saving ? "保存中…" : "保存" }}</button>
      </template>
    </ModalDialog>

    <ModalDialog :open="Boolean(pendingDelete)" :title="`删除${noun}`" @close="pendingDelete = null">
      <p class="confirm-text">确定删除{{ noun }} <strong>{{ pendingDelete ? itemKey(pendingDelete) : "" }}</strong> 吗？</p>
      <template #footer>
        <button type="button" class="af-button secondary" @click="pendingDelete = null">取消</button>
        <button type="button" class="af-button danger" :disabled="deleting" @click="confirmDelete">{{ deleting ? "删除中…" : "确认删除" }}</button>
      </template>
    </ModalDialog>
  </div>
</template>

<style scoped>
.manager {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.manager-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.head-note { font-size: 12px; }
.row-actions {
  display: flex;
  align-items: center;
  gap: 2px;
}
.match-cell {
  color: var(--af-muted);
  font-size: 11px;
  line-height: 1.6;
  white-space: normal;
}
.form-stack {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.check-label {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--af-muted);
  font-size: 13px;
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
</style>
