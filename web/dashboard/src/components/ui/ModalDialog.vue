<script setup lang="ts">
import { onBeforeUnmount, watch } from "vue";

const props = withDefaults(defineProps<{ open: boolean; title?: string; width?: string }>(), { width: "480px" });
const emit = defineEmits<{ close: [] }>();

function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") emit("close");
}

watch(
  () => props.open,
  (open) => {
    if (open) document.addEventListener("keydown", onKeydown);
    else document.removeEventListener("keydown", onKeydown);
  },
);
onBeforeUnmount(() => document.removeEventListener("keydown", onKeydown));
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div v-if="open" class="modal-mask" @click.self="emit('close')">
        <div class="modal" :style="{ maxWidth: width }" role="dialog" aria-modal="true">
          <header v-if="title" class="modal-head">
            <h2 class="modal-title">{{ title }}</h2>
            <button type="button" class="modal-close" aria-label="关闭" @click="emit('close')">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
                <path d="M18 6 6 18M6 6l12 12" />
              </svg>
            </button>
          </header>
          <div class="modal-body">
            <slot />
          </div>
          <footer v-if="$slots.footer" class="modal-foot">
            <slot name="footer" />
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.modal-mask {
  position: fixed;
  z-index: 50;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 20px;
  background: var(--af-mask);
}
.modal {
  width: 100%;
  max-height: calc(100vh - 80px);
  overflow-y: auto;
  padding: 20px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius-lg);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-lg);
}
.modal-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 16px;
}
.modal-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
}
.modal-close {
  display: grid;
  width: 28px;
  height: 28px;
  place-items: center;
  border-radius: 6px;
  color: var(--af-faint);
  transition: background 120ms ease, color 120ms ease;
}
.modal-close:hover { color: var(--af-text); background: var(--af-panel-soft); }
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 20px;
}
.modal-enter-active, .modal-leave-active { transition: opacity 180ms ease; }
.modal-enter-active .modal, .modal-leave-active .modal { transition: transform 180ms ease, opacity 180ms ease; }
.modal-enter-from, .modal-leave-to { opacity: 0; }
.modal-enter-from .modal, .modal-leave-to .modal { opacity: 0; transform: translateY(10px) scale(0.98); }
</style>
