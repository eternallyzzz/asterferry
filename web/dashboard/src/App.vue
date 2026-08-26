<script setup lang="ts">
import { provide, ref, watch } from "vue";
import { ConfigProvider } from "@arco-design/web-vue";
import DashboardShell from "./components/DashboardShell.vue";
import TokenGate from "./components/TokenGate.vue";
import { dashboardKey } from "./dashboard-context";
import { useDashboard } from "./dashboard";
import { useSession } from "./session";

const session = useSession();
const viewerToken = session.viewerToken;
const viewerError = session.viewerError;
const theme = ref<"dark" | "light">(window.localStorage.getItem("asterferry.dashboard.theme") === "light" ? "light" : "dark");
const dashboard = useDashboard(viewerToken, session.invalidateViewer);

provide(dashboardKey, dashboard);

watch(theme, (value) => {
  document.documentElement.dataset.theme = value;
  document.body.classList.toggle("arco-theme-dark", value === "dark");
  document.body.setAttribute("arco-theme", value);
  window.localStorage.setItem("asterferry.dashboard.theme", value);
}, { immediate: true });
</script>

<template>
  <ConfigProvider size="medium">
    <TokenGate
      v-if="!viewerToken"
      :theme="theme"
      :error="viewerError"
      @unlock="session.unlock"
      @toggle-theme="theme = theme === 'dark' ? 'light' : 'dark'"
    />
    <DashboardShell
      v-else
      :theme="theme"
      @lock="session.lock"
      @toggle-theme="theme = theme === 'dark' ? 'light' : 'dark'"
    />
  </ConfigProvider>
</template>
