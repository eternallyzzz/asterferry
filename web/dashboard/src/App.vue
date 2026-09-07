<script setup lang="ts">
import { onMounted, ref } from "vue";
import AppShell from "./components/shell/AppShell.vue";
import ThemeToggle from "./components/ui/ThemeToggle.vue";
import { useSession } from "./session";
import { useTheme } from "./composables/useTheme";

const session = useSession();
const controllerUser = session.controllerUser;
const controllerError = session.controllerError;
const { theme, toggle } = useTheme();

const username = ref("");
const password = ref("");
const submitting = ref(false);

onMounted(() => {
  void session.restore();
});

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
  <main v-if="!controllerUser" class="auth-shell">
    <section class="auth-card">
      <div class="auth-brand">
        <span class="brand-mark">
          <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">
            <path d="m16 3 4 4-4 4" />
            <path d="M20 7H4" />
            <path d="m8 21-4-4 4-4" />
            <path d="M4 17h16" />
          </svg>
        </span>
      </div>
      <h1 class="auth-title">登录 AsterFerry</h1>
      <p class="auth-sub">Controller 控制面 · 节点、服务、调度与审计</p>
      <form class="auth-form" @submit.prevent="submit">
        <label class="auth-field">
          <span>用户名</span>
          <input v-model="username" name="username" autocomplete="username" autofocus required />
        </label>
        <label class="auth-field">
          <span>密码</span>
          <input v-model="password" name="password" type="password" autocomplete="current-password" required />
        </label>
        <p v-if="controllerError" class="auth-error">{{ controllerError }}</p>
        <button class="af-button primary auth-submit" type="submit" :disabled="submitting">{{ submitting ? "登录中…" : "登录" }}</button>
      </form>
    </section>
    <div class="auth-theme">
      <ThemeToggle :theme="theme" @toggle="toggle" />
    </div>
  </main>
  <AppShell v-else :user="controllerUser" @logout="logout" />
</template>

<style scoped>
.auth-shell {
  position: relative;
  display: grid;
  min-height: 100vh;
  place-items: center;
  padding: 24px;
  background: radial-gradient(900px 420px at 50% -8%, var(--af-accent-soft), transparent 65%), var(--af-bg);
}
.auth-card {
  width: min(100%, 400px);
  padding: 36px 32px 32px;
  border: 1px solid var(--af-border);
  border-radius: var(--af-radius-lg);
  background: var(--af-panel);
  box-shadow: var(--af-shadow);
}
.auth-brand { display: flex; justify-content: center; }
.brand-mark {
  display: grid;
  width: 44px;
  height: 44px;
  place-items: center;
  border-radius: 12px;
  color: var(--af-on-accent);
  background: var(--af-accent);
}
.auth-title {
  margin: 20px 0 6px;
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.02em;
  text-align: center;
}
.auth-sub {
  margin: 0 0 24px;
  color: var(--af-muted);
  font-size: 12px;
  text-align: center;
}
.auth-form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.auth-field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.auth-field span {
  color: var(--af-muted);
  font-size: 12px;
  font-weight: 500;
}
.auth-field input { height: 40px; }
.auth-error {
  margin: 0;
  color: var(--af-red);
  font-size: 12px;
}
.auth-submit {
  width: 100%;
  height: 40px;
  margin-top: 4px;
}
.auth-theme {
  position: absolute;
  right: 20px;
  bottom: 20px;
}
</style>
