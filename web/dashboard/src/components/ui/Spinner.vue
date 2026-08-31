<script setup lang="ts">
withDefaults(defineProps<{ size?: number }>(), { size: 16 });
</script>

<template>
  <span class="spinner" :style="{ width: size + 'px', height: size + 'px' }" role="status" aria-label="加载中" />
</template>

<style scoped>
.spinner {
  display: inline-block;
  border: 2px solid var(--af-border);
  border-top-color: var(--af-accent);
  border-radius: 50%;
  animation: af-spin 0.7s linear infinite;
}
</style>
