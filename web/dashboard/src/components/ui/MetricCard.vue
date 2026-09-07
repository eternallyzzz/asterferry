<script setup lang="ts">
withDefaults(defineProps<{ label: string; value: string | number; tone?: "default" | "good" | "warn" | "bad" }>(), { tone: "default" });
</script>

<template>
  <article class="metric">
    <span class="metric-label">{{ label }}</span>
    <strong class="metric-value" :data-tone="tone">{{ value }}</strong>
  </article>
</template>

<style scoped>
.metric {
  display: flex;
  min-width: 0;
  min-height: 92px;
  flex-direction: column;
  justify-content: space-between;
  gap: 12px;
  padding: 16px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-sm);
}
.metric-label {
  color: var(--af-muted);
  font-size: 12px;
}
.metric-value {
  font-size: 28px;
  font-weight: 600;
  letter-spacing: -0.03em;
  line-height: 1.1;
}
.metric-value[data-tone="good"] { color: var(--af-green); }
.metric-value[data-tone="warn"] { color: var(--af-amber); }
.metric-value[data-tone="bad"] { color: var(--af-red); }
</style>
