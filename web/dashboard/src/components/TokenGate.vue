<script setup lang="ts">
import { ref } from "vue";
import { Alert, Button, Card, InputPassword, Typography } from "@arco-design/web-vue";

defineProps<{ theme: "dark" | "light"; error?: string }>();
const emit = defineEmits<{ unlock: [token: string]; toggleTheme: [] }>();
const draft = ref("");

function submit() {
  if (draft.value.trim()) emit("unlock", draft.value.trim());
}
</script>

<template>
  <main class="auth-shell">
    <Card class="auth-card" :bordered="false">
      <div class="brand-mark">AF</div>
      <p class="eyebrow">ASTERFERRY / PRIVATE MANAGEMENT</p>
      <Typography.Title :heading="2">打开管理面板</Typography.Title>
      <Typography.Paragraph class="muted">
        Dashboard 只通过当前管理地址访问。Viewer token 只保存在浏览器内存，不会写入 URL 或磁盘。
      </Typography.Paragraph>
      <Alert v-if="error" type="error" show-icon class="auth-error">{{ error }}</Alert>
      <form class="auth-form" @submit.prevent="submit">
        <label for="viewer-token">Viewer token</label>
        <InputPassword id="viewer-token" v-model="draft" allow-clear autofocus placeholder="粘贴 secrets/ 中的 Viewer token" />
        <Button type="primary" html-type="submit" long :disabled="!draft.trim()">解锁 Dashboard</Button>
      </form>
      <Button type="text" class="theme-button" @click="emit('toggleTheme')">
        使用{{ theme === "dark" ? "浅色" : "深色" }}主题
      </Button>
    </Card>
  </main>
</template>
