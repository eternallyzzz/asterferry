<script setup lang="ts">
import { ref, watch } from "vue";
import { Button, InputPassword, Modal, Typography } from "@arco-design/web-vue";

const props = defineProps<{ visible: boolean; busy?: boolean; error?: string }>();
const emit = defineEmits<{ "update:visible": [value: boolean]; confirm: [token: string]; cancel: [] }>();
const draft = ref("");

watch(() => props.visible, (visible) => {
  if (visible) draft.value = "";
});

function close() {
  emit("update:visible", false);
  emit("cancel");
}

function confirm() {
  if (draft.value.trim()) emit("confirm", draft.value.trim());
}
</script>

<template>
  <Modal
    :visible="visible"
    title="需要 Admin token"
    :footer="false"
    unmount-on-close
    @cancel="close"
    @update:visible="emit('update:visible', $event)"
  >
    <Typography.Paragraph class="muted">
      这是配置写操作。Admin token 只会保存在当前页面内存中，锁定或刷新后自动清除。
    </Typography.Paragraph>
    <InputPassword v-model="draft" autofocus allow-clear placeholder="粘贴 Admin token" @press-enter="confirm" />
    <p v-if="error" class="form-error">{{ error }}</p>
    <div class="modal-actions">
      <Button @click="close">取消</Button>
      <Button type="primary" :loading="busy" :disabled="!draft.trim()" @click="confirm">继续</Button>
    </div>
  </Modal>
</template>
