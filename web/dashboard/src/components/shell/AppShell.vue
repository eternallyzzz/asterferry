<script setup lang="ts">
import AppHeader from "./AppHeader.vue";
import ToastHost from "../ui/ToastHost.vue";
import type { ControllerUser } from "../../controller-api";

defineProps<{ user: ControllerUser }>();
const emit = defineEmits<{ logout: [] }>();
</script>

<template>
  <div class="app-shell">
    <AppHeader :user="user" @logout="emit('logout')" />
    <main class="app-main">
      <router-view />
    </main>
    <ToastHost />
  </div>
</template>

<style scoped>
.app-shell { min-height: 100vh; }
.app-main {
  max-width: 1200px;
  margin: 0 auto;
  padding: 32px 20px 56px;
}
@media (max-width: 640px) {
  .app-main { padding: 20px 14px 40px; }
}
</style>
