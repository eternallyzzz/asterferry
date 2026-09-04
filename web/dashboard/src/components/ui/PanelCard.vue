<script setup lang="ts">
defineProps<{ title?: string }>();
</script>

<template>
  <section class="panel">
    <header v-if="title || $slots.actions" class="panel-head">
      <h2 v-if="title" class="panel-title">{{ title }}</h2>
      <div v-if="$slots.actions" class="panel-actions">
        <slot name="actions" />
      </div>
    </header>
    <slot />
  </section>
</template>

<style scoped>
.panel {
  min-width: 0;
  padding: 20px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-sm);
}
.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 16px;
}
.panel-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.panel-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--af-muted);
  font-size: 12px;
}
@media (max-width: 640px) {
  .panel { padding: 16px; }
}
</style>
