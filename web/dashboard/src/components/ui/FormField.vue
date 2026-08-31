<script setup lang="ts">
defineProps<{ label: string; hint?: string; error?: string }>();
</script>

<template>
  <label class="field">
    <span class="field-label">{{ label }}</span>
    <slot />
    <span v-if="error" class="field-error">{{ error }}</span>
    <span v-else-if="hint" class="field-hint">{{ hint }}</span>
  </label>
</template>

<style scoped>
.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}
.field-label {
  color: var(--af-muted);
  font-size: 12px;
  font-weight: 500;
}
.field-error {
  color: var(--af-red);
  font-size: 12px;
}
.field-hint {
  color: var(--af-faint);
  font-size: 12px;
  line-height: 1.6;
}
</style>
