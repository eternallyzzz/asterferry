<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";

const props = defineProps<{ username: string; role: string }>();
const emit = defineEmits<{ logout: [] }>();

const open = ref(false);
const root = ref<HTMLElement | null>(null);

const initial = computed(() => (props.username.trim()[0] || "?").toUpperCase());
const roleText = computed(() => ({ viewer: "Viewer · 只读", operator: "Operator · 运维", admin: "Admin · 管理员" } as Record<string, string>)[props.role] || props.role);

function onDocumentClick(event: MouseEvent) {
  if (root.value && !root.value.contains(event.target as Node)) open.value = false;
}

watch(open, (value) => {
  if (value) document.addEventListener("click", onDocumentClick);
  else document.removeEventListener("click", onDocumentClick);
});
onBeforeUnmount(() => document.removeEventListener("click", onDocumentClick));

function logout() {
  open.value = false;
  emit("logout");
}
</script>

<template>
  <div ref="root" class="user-menu">
    <button type="button" class="trigger" :aria-expanded="open" aria-haspopup="menu" @click="open = !open">
      <span class="avatar">{{ initial }}</span>
      <span class="name">{{ username }}</span>
      <svg class="chevron" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="m6 9 6 6 6-6" />
      </svg>
    </button>
    <Transition name="menu">
      <div v-if="open" class="menu" role="menu">
        <div class="menu-head">
          <strong>{{ username }}</strong>
          <span>{{ roleText }}</span>
        </div>
        <button type="button" class="menu-item danger" role="menuitem" @click="logout">退出登录</button>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.user-menu { position: relative; }
.trigger {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 8px 4px 4px;
  border-radius: 999px;
  transition: background 120ms ease;
}
.trigger:hover { background: var(--af-panel-soft); }
.avatar {
  display: grid;
  width: 26px;
  height: 26px;
  place-items: center;
  border-radius: 50%;
  color: var(--af-on-accent);
  background: var(--af-accent);
  font-size: 12px;
  font-weight: 600;
}
.name {
  max-width: 120px;
  overflow: hidden;
  font-size: 13px;
  font-weight: 500;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.chevron { color: var(--af-faint); }
.menu {
  position: absolute;
  z-index: 45;
  top: calc(100% + 6px);
  right: 0;
  width: 200px;
  padding: 6px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-lg);
}
.menu-head {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px 10px 10px;
  border-bottom: 1px solid var(--af-border-soft);
  margin-bottom: 4px;
}
.menu-head strong { font-size: 13px; }
.menu-head span { color: var(--af-faint); font-size: 11px; }
.menu-item {
  display: block;
  width: 100%;
  padding: 7px 10px;
  border-radius: 6px;
  font-size: 13px;
  text-align: left;
  transition: background 120ms ease;
}
.menu-item.danger { color: var(--af-red); }
.menu-item.danger:hover { background: var(--af-red-soft); }
.menu-enter-active, .menu-leave-active { transition: opacity 140ms ease, transform 140ms ease; }
.menu-enter-from, .menu-leave-to { opacity: 0; transform: translateY(-4px); }
@media (max-width: 640px) {
  .name, .chevron { display: none; }
}
</style>
