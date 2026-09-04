<script setup lang="ts">
defineProps<{ title?: string; description?: string }>();
</script>

<template>
  <div class="empty-state">
    <slot />
    <p v-if="title" class="empty-title">{{ title }}</p>
    <p v-if="description" class="empty-desc">{{ description }}</p>
    <div v-if="$slots.action" class="empty-action">
      <slot name="action" />
    </div>
  </div>
</template>

<style scoped>
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 36px 20px;
  text-align: center;
}
.empty-title {
  margin: 0;
  color: var(--af-muted);
  font-size: 13px;
  font-weight: 600;
}
.empty-desc {
  margin: 0;
  max-width: 420px;
  color: var(--af-faint);
  font-size: 12px;
  line-height: 1.7;
}
.empty-action { margin-top: 10px; }
</style>
