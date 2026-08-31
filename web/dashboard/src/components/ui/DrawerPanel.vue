<script setup lang="ts">
import { onBeforeUnmount, watch } from "vue";

const props = defineProps<{ open: boolean; title?: string }>();
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
    <Transition name="fade">
      <div v-if="open" class="drawer-mask" @click.self="emit('close')" />
    </Transition>
    <Transition name="drawer">
      <aside v-if="open" class="drawer" role="dialog" aria-modal="true">
        <header class="drawer-head">
          <h2 v-if="title" class="drawer-title">{{ title }}</h2>
          <button type="button" class="drawer-close" aria-label="关闭" @click="emit('close')">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
              <path d="M18 6 6 18M6 6l12 12" />
            </svg>
          </button>
        </header>
        <div class="drawer-body">
          <slot />
        </div>
      </aside>
    </Transition>
  </Teleport>
</template>

<style scoped>
.drawer-mask {
  position: fixed;
  z-index: 50;
  inset: 0;
  background: var(--af-mask);
}
.drawer {
  position: fixed;
  z-index: 51;
  top: 0;
  right: 0;
  bottom: 0;
  display: flex;
  width: min(720px, 100vw);
  flex-direction: column;
  border-left: 1px solid var(--af-border);
  background: var(--af-bg);
  box-shadow: var(--af-shadow-lg);
}
.drawer-head {
  display: flex;
  flex: 0 0 auto;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 16px 20px;
  border-bottom: 1px solid var(--af-border);
  background: var(--af-header-bg);
  backdrop-filter: blur(20px);
  -webkit-backdrop-filter: blur(20px);
}
.drawer-title {
  margin: 0;
  overflow: hidden;
  font-size: 15px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.drawer-close {
  display: grid;
  width: 28px;
  height: 28px;
  flex: 0 0 auto;
  place-items: center;
  border-radius: 6px;
  color: var(--af-faint);
  transition: background 120ms ease, color 120ms ease;
}
.drawer-close:hover { color: var(--af-text); background: var(--af-panel-soft); }
.drawer-body {
  flex: 1 1 auto;
  overflow-y: auto;
  padding: 20px;
}
.fade-enter-active, .fade-leave-active { transition: opacity 200ms ease; }
.fade-enter-from, .fade-leave-to { opacity: 0; }
.drawer-enter-active, .drawer-leave-active { transition: transform 200ms ease; }
.drawer-enter-from, .drawer-leave-to { transform: translateX(100%); }
</style>
