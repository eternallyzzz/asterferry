<script setup lang="ts">
import { computed } from "vue";
import { RouterLink } from "vue-router";
import ThemeToggle from "../ui/ThemeToggle.vue";
import UserMenu from "../ui/UserMenu.vue";
import { useTheme } from "../../composables/useTheme";
import { useSession } from "../../session";
import type { ControllerUser } from "../../controller-api";

defineProps<{ user: ControllerUser }>();
const emit = defineEmits<{ logout: [] }>();

const { theme, toggle } = useTheme();
const session = useSession();

const navItems = computed(() => {
  const items = [
    { to: "/overview", label: "概览" },
    { to: "/nodes", label: "节点" },
    { to: "/services", label: "服务" },
    { to: "/assignments", label: "调度" },
    { to: "/activity", label: "活动" },
  ];
  if (session.canAdmin.value) items.push({ to: "/admin", label: "管理" });
  return items;
});
</script>

<template>
  <header class="app-header">
    <div class="header-inner">
      <RouterLink to="/overview" class="brand" aria-label="AsterFerry 概览">
        <span class="brand-mark">
          <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
            <path d="m16 3 4 4-4 4" />
            <path d="M20 7H4" />
            <path d="m8 21-4-4 4-4" />
            <path d="M4 17h16" />
          </svg>
        </span>
        <span class="brand-text">
          <strong>AsterFerry</strong>
          <small>Controller</small>
        </span>
      </RouterLink>
      <nav class="main-nav" aria-label="主导航">
        <RouterLink v-for="item in navItems" :key="item.to" :to="item.to" class="nav-link">{{ item.label }}</RouterLink>
      </nav>
      <div class="header-side">
        <ThemeToggle :theme="theme" @toggle="toggle" />
        <UserMenu :username="user.username" :role="user.role" @logout="emit('logout')" />
      </div>
    </div>
  </header>
</template>

<style scoped>
.app-header {
  position: sticky;
  top: 0;
  z-index: 40;
  border-bottom: 1px solid var(--af-border);
  background: var(--af-header-bg);
  backdrop-filter: blur(20px) saturate(180%);
  -webkit-backdrop-filter: blur(20px) saturate(180%);
}
.header-inner {
  display: flex;
  align-items: center;
  gap: 16px;
  max-width: 1200px;
  height: 56px;
  margin: 0 auto;
  padding: 0 20px;
}
.brand {
  display: flex;
  flex: 1 1 0;
  min-width: 0;
  align-items: center;
  gap: 10px;
  color: var(--af-text);
}
.brand-mark {
  display: grid;
  width: 28px;
  height: 28px;
  flex: 0 0 auto;
  place-items: center;
  border-radius: 8px;
  color: var(--af-on-accent);
  background: var(--af-accent);
}
.brand-text {
  display: flex;
  min-width: 0;
  flex-direction: column;
  line-height: 1.25;
}
.brand-text strong {
  font-size: 13px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.brand-text small {
  color: var(--af-faint);
  font-size: 10px;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.main-nav {
  display: flex;
  flex: 0 1 auto;
  align-items: center;
  gap: 2px;
  padding: 3px;
  border: 1px solid var(--af-border);
  border-radius: 999px;
  background: var(--af-panel-soft);
  overflow-x: auto;
  scrollbar-width: none;
}
.main-nav::-webkit-scrollbar { display: none; }
.nav-link {
  padding: 4px 12px;
  border-radius: 999px;
  color: var(--af-muted);
  font-size: 13px;
  font-weight: 500;
  white-space: nowrap;
  transition: color 120ms ease, background 120ms ease;
}
.nav-link:hover { color: var(--af-text); }
.nav-link.router-link-active {
  color: var(--af-text);
  background: var(--af-panel);
  box-shadow: var(--af-shadow-sm);
}
.header-side {
  display: flex;
  flex: 1 1 0;
  align-items: center;
  justify-content: flex-end;
  gap: 6px;
}
@media (max-width: 900px) {
  .brand-text { display: none; }
  .brand { flex: 0 0 auto; }
  .header-side { flex: 0 0 auto; margin-left: auto; }
}
</style>
