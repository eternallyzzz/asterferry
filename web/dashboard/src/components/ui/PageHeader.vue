<script setup lang="ts">
defineProps<{ eyebrow?: string; title: string; description?: string }>();
</script>

<template>
  <div class="page-header">
    <div class="page-header-text">
      <p v-if="eyebrow" class="eyebrow">{{ eyebrow }}</p>
      <h1 class="title">{{ title }}</h1>
      <p v-if="description" class="description">{{ description }}</p>
    </div>
    <div v-if="$slots.actions" class="actions">
      <slot name="actions" />
    </div>
  </div>
</template>

<style scoped>
.page-header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 20px;
}
.page-header-text { min-width: 0; }
.eyebrow {
  margin: 0 0 6px;
  color: var(--af-accent);
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.12em;
  text-transform: uppercase;
}
.title {
  margin: 0;
  font-size: 24px;
  font-weight: 600;
  letter-spacing: -0.02em;
  line-height: 1.3;
}
.description {
  margin: 6px 0 0;
  max-width: 640px;
  color: var(--af-muted);
  font-size: 13px;
}
.actions {
  display: flex;
  flex: 0 0 auto;
  align-items: center;
  gap: 8px;
}
@media (max-width: 640px) {
  .page-header { align-items: flex-start; flex-direction: column; gap: 14px; }
}
</style>
