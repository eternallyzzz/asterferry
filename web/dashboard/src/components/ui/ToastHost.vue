<script setup lang="ts">
import { useNotify } from "../../composables/useNotify";

const { toasts, dismiss } = useNotify();
</script>

<template>
  <div class="toast-host" aria-live="polite">
    <TransitionGroup name="toast">
      <button v-for="toast in toasts" :key="toast.id" type="button" class="toast" :data-tone="toast.tone" @click="dismiss(toast.id)">
        {{ toast.message }}
      </button>
    </TransitionGroup>
  </div>
</template>

<style scoped>
.toast-host {
  position: fixed;
  z-index: 60;
  top: 68px;
  right: 20px;
  display: flex;
  width: min(360px, calc(100vw - 40px));
  flex-direction: column;
  gap: 8px;
  pointer-events: none;
}
.toast {
  display: block;
  width: 100%;
  padding: 10px 14px;
  border: 1px solid var(--af-border);
  border-left: 3px solid var(--af-accent);
  border-radius: var(--af-radius-sm);
  background: var(--af-panel);
  box-shadow: var(--af-shadow);
  color: var(--af-text);
  font-size: 13px;
  line-height: 1.5;
  text-align: left;
  pointer-events: auto;
}
.toast[data-tone="success"] { border-left-color: var(--af-green); }
.toast[data-tone="error"] { border-left-color: var(--af-red); }
.toast-enter-active, .toast-leave-active { transition: opacity 160ms ease, transform 160ms ease; }
.toast-enter-from, .toast-leave-to { opacity: 0; transform: translateY(-6px); }
</style>
