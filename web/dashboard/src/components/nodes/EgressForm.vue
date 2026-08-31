<script setup lang="ts">
import { ref, watch } from "vue";
import FormField from "../ui/FormField.vue";
import { updateAgentEgress, updateGatewayEgress, type EgressPolicy } from "../../controller-api";
import { useNotify } from "../../composables/useNotify";
import { useSession } from "../../session";
import { describeError, joinList, newIdempotencyKey, splitList } from "../../utils/format";

const props = defineProps<{
  kind: "gateway" | "agent";
  nodeId: string;
  revision: number | undefined;
  initial?: EgressPolicy;
}>();
const emit = defineEmits<{ saved: [] }>();

const notify = useNotify();
const session = useSession();
const saving = ref(false);
const error = ref("");

const form = ref({ enabled: false, tcp: "", udp: "", cidrs: "", special: "", max: "0" });

watch(
  () => props.initial,
  (policy) => {
    form.value = {
      enabled: policy?.enabled ?? false,
      tcp: joinList(policy?.tcp_ports),
      udp: joinList(policy?.udp_ports),
      cidrs: joinList(policy?.allow_cidrs),
      special: joinList(policy?.allow_special_cidrs),
      max: String(policy?.max_connections ?? 0),
    };
  },
  { immediate: true },
);

async function save() {
  if (props.revision === undefined) {
    error.value = "请先在「规格」分段保存规格文档，再编辑出口策略。";
    return;
  }
  saving.value = true;
  error.value = "";
  try {
    const policy: EgressPolicy = {
      enabled: form.value.enabled,
      tcp_ports: splitList(form.value.tcp),
      udp_ports: splitList(form.value.udp),
      allow_cidrs: splitList(form.value.cidrs),
      allow_special_cidrs: splitList(form.value.special),
      max_connections: Number(form.value.max) || 0,
    };
    const update = props.kind === "gateway" ? updateGatewayEgress : updateAgentEgress;
    await update(props.nodeId, policy, props.revision, undefined, newIdempotencyKey());
    notify.success("出口策略已保存。");
    emit("saved");
  } catch (caught) {
    error.value = describeError(caught);
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <form class="egress-form" @submit.prevent="save">
    <p class="muted-note">启用后仅允许向列出的端口段与 CIDR 发起出站连接；留空端口段表示不限制该协议。</p>
    <label class="check-label">
      <input v-model="form.enabled" type="checkbox" /> 启用出口策略
    </label>
    <div class="form-grid">
      <FormField label="TCP 端口段" hint='逗号分隔，如 "443, 8000-8080"'>
        <input v-model="form.tcp" spellcheck="false" placeholder="443, 8000-8080" />
      </FormField>
      <FormField label="UDP 端口段" hint='逗号分隔，如 "53, 5000-5010"'>
        <input v-model="form.udp" spellcheck="false" placeholder="53" />
      </FormField>
      <FormField label="允许的 CIDR" hint="逗号分隔；留空表示不限制">
        <input v-model="form.cidrs" spellcheck="false" placeholder="10.0.0.0/8, 192.168.0.0/16" />
      </FormField>
      <FormField label="特殊 CIDR 例外" hint="如 localhost、link-local 等后端约定的特殊段">
        <input v-model="form.special" spellcheck="false" placeholder="localhost" />
      </FormField>
      <FormField label="最大连接数" hint="0 表示不限制">
        <input v-model="form.max" type="number" min="0" max="1048576" />
      </FormField>
    </div>
    <p v-if="error" class="form-error">{{ error }}</p>
    <div class="form-foot">
      <button type="submit" class="af-button primary" :disabled="!session.canOperate.value || saving">{{ saving ? "保存中…" : "保存出口策略" }}</button>
    </div>
  </form>
</template>

<style scoped>
.egress-form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.muted-note {
  margin: 0;
  color: var(--af-faint);
  font-size: 12px;
  line-height: 1.6;
}
.check-label {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--af-muted);
  font-size: 13px;
}
.form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}
.form-error {
  margin: 0;
  color: var(--af-red);
  font-size: 12px;
}
.form-foot {
  display: flex;
  justify-content: flex-end;
}
@media (max-width: 640px) {
  .form-grid { grid-template-columns: 1fr; }
}
</style>
