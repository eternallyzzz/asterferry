<script setup lang="ts">
withDefaults(defineProps<{ tone?: "good" | "warn" | "bad" | "neutral" }>(), { tone: "neutral" });
</script>

<template>
  <span class="pill" :data-tone="tone"><slot /></span>
</template>

<style scoped>
.pill {
  display: inline-flex;
  align-items: center;
  padding: 2px 8px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 500;
  line-height: 18px;
  white-space: nowrap;
}
.pill[data-tone="good"] { color: var(--af-green); background: var(--af-green-soft); }
.pill[data-tone="warn"] { color: var(--af-amber); background: var(--af-amber-soft); }
.pill[data-tone="bad"] { color: var(--af-red); background: var(--af-red-soft); }
.pill[data-tone="neutral"] { color: var(--af-muted); background: var(--af-panel-soft); }
</style>
