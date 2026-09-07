<script setup lang="ts">
defineProps<{ empty?: boolean }>();
</script>

<template>
  <div class="table-wrap">
    <table class="data-table">
      <slot />
    </table>
  </div>
  <div v-if="empty" class="table-empty">
    <slot name="empty" />
  </div>
</template>

<style scoped>
.table-wrap { overflow-x: auto; }
.data-table {
  width: 100%;
  min-width: 640px;
  border-collapse: collapse;
}
.data-table :deep(th) {
  padding: 0 12px 10px;
  border-bottom: 1px solid var(--af-border);
  color: var(--af-faint);
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.08em;
  text-align: left;
  text-transform: uppercase;
  white-space: nowrap;
}
.data-table :deep(td) {
  padding: 12px;
  border-bottom: 1px solid var(--af-border-soft);
  color: var(--af-text);
  font-size: 13px;
  text-align: left;
  vertical-align: middle;
}
.data-table :deep(tbody tr:last-child td) { border-bottom: 0; }
.data-table :deep(tbody tr) { transition: background 120ms ease; }
.data-table :deep(tbody tr:hover) { background: var(--af-panel-soft); }
.data-table :deep(td code) { color: var(--af-muted); font-size: 12px; }
.data-table :deep(td small) {
  display: block;
  margin-top: 2px;
  color: var(--af-faint);
  font-size: 11px;
}
.table-empty {
  padding: 8px 0 4px;
}
</style>
