<script setup lang="ts">
import { onMounted, ref } from "vue";
import EmptyState from "../ui/EmptyState.vue";
import Spinner from "../ui/Spinner.vue";
import { ControllerAPIError, getSnapshot, type ControllerSnapshot } from "../../controller-api";
import { useNotify } from "../../composables/useNotify";
import { describeError, prettyJson } from "../../utils/format";

const props = defineProps<{ nodeId: string }>();

const notify = useNotify();
const loading = ref(true);
const absent = ref(false);
const snapshot = ref<ControllerSnapshot | null>(null);

onMounted(async () => {
  try {
    snapshot.value = await getSnapshot(props.nodeId);
  } catch (caught) {
    if (caught instanceof ControllerAPIError && caught.status === 404) absent.value = true;
    else notify.error(describeError(caught));
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div v-if="loading" class="loading-row"><Spinner :size="18" /></div>
  <EmptyState v-else-if="absent" title="暂无快照" description="Controller 会为有完整配置的节点生成期望快照。" />
  <div v-else-if="snapshot" class="snapshot">
    <dl class="meta-grid">
      <div><dt>Generation</dt><dd>{{ snapshot.generation }}</dd></div>
      <div><dt>Schema</dt><dd>{{ snapshot.schema_version }}</dd></div>
      <div class="span-2"><dt>Checksum (SHA-256)</dt><dd><code>{{ snapshot.checksum }}</code></dd></div>
    </dl>
    <pre class="snapshot-json mono">{{ prettyJson(snapshot) }}</pre>
  </div>
</template>

<style scoped>
.loading-row {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 160px;
}
.snapshot {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.meta-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px 20px;
  margin: 0;
}
.meta-grid .span-2 { grid-column: span 2; }
.meta-grid dt {
  color: var(--af-faint);
  font-size: 11px;
}
.meta-grid dd {
  margin: 3px 0 0;
  font-size: 13px;
  overflow-wrap: anywhere;
}
.meta-grid code {
  color: var(--af-muted);
  font-size: 11px;
}
.snapshot-json {
  max-height: 420px;
  overflow: auto;
  margin: 0;
  padding: 12px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius-sm);
  background: var(--af-panel-soft);
  color: var(--af-muted);
  font-size: 11px;
  line-height: 1.6;
}
</style>
