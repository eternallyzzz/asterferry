<script setup lang="ts">
import { onMounted, ref, watch } from "vue";
import { ConfigProvider } from "@arco-design/web-vue";
import ControllerShell from "./components/ControllerShell.vue";
import { useSession } from "./session";

const session = useSession();
const controllerUser = session.controllerUser;
const controllerError = session.controllerError;
const viewerError = session.viewerError;
const username = ref("");
const password = ref("");
const submitting = ref(false);
const theme = ref<"dark" | "light">(window.localStorage.getItem("asterferry.dashboard.theme") === "light" ? "light" : "dark");

onMounted(() => {
  void session.restore();
});

watch(theme, (value) => {
  document.documentElement.dataset.theme = value;
  document.body.classList.toggle("arco-theme-dark", value === "dark");
  document.body.setAttribute("arco-theme", value);
  window.localStorage.setItem("asterferry.dashboard.theme", value);
}, { immediate: true });

async function submit() {
  submitting.value = true;
  try {
    await session.login(username.value, password.value);
    password.value = "";
  } catch {
    // The session composable exposes the sanitized error to the template.
  } finally {
    submitting.value = false;
  }
}

async function logout() {
  try {
    await session.logout();
  } catch {
    session.lock();
  }
}
</script>

<template>
  <ConfigProvider size="medium">
    <main v-if="!controllerUser" class="auth-shell">
      <section class="auth-card">
        <div class="section-kicker">ASTERFERRY CONTROLLER</div>
        <h1>登录控制面</h1>
        <p class="muted">使用 Controller 账户管理节点、服务、调度和审计。</p>
        <form class="auth-form" @submit.prevent="submit">
          <label>用户名<input v-model="username" name="username" autocomplete="username" required /></label>
          <label>密码<input v-model="password" name="password" type="password" autocomplete="current-password" required /></label>
          <p v-if="controllerError || viewerError" class="auth-error">{{ controllerError || viewerError }}</p>
          <button class="primary-button" type="submit" :disabled="submitting">{{ submitting ? "登录中…" : "登录" }}</button>
        </form>
        <button class="theme-toggle" type="button" @click="theme = theme === 'dark' ? 'light' : 'dark'">切换{{ theme === 'dark' ? "浅色" : "深色" }}主题</button>
      </section>
    </main>
    <ControllerShell
      v-else
      :user="controllerUser"
      @logout="logout"
    />
  </ConfigProvider>
</template>
