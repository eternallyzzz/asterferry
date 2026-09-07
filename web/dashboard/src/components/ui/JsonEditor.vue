<script setup lang="ts">
import { ref, watch } from "vue";

const props = withDefaults(
  defineProps<{ modelValue: string; rows?: number; placeholder?: string; footer?: string }>(),
  { rows: 14, placeholder: "", footer: "" },
);
const emit = defineEmits<{ "update:modelValue": [value: string]; validity: [valid: boolean] }>();

const error = ref("");

watch(
  () => props.modelValue,
  (value) => {
    if (!value.trim()) {
      error.value = "";
      emit("validity", true);
      return;
    }
    try {
      JSON.parse(value);
      error.value = "";
      emit("validity", true);
    } catch {
      error.value = "JSON 格式错误，请检查括号、逗号与引号。";
      emit("validity", false);
    }
  },
  { immediate: true },
);

function onInput(event: Event) {
  emit("update:modelValue", (event.target as HTMLTextAreaElement).value);
}
</script>

<template>
  <div class="json-editor">
    <textarea
      class="editor"
      :class="{ invalid: error }"
      :rows="rows"
      :placeholder="placeholder"
      spellcheck="false"
      :value="modelValue"
      @input="onInput"
    />
    <div v-if="error || footer" class="editor-foot">
      <span v-if="error" class="editor-error">{{ error }}</span>
      <span v-else class="editor-hint">{{ footer }}</span>
    </div>
  </div>
</template>

<style scoped>
.json-editor { min-width: 0; }
.editor {
  display: block;
  width: 100%;
  padding: 12px;
  resize: vertical;
  font-family: var(--af-mono);
  font-size: 12px;
  line-height: 1.6;
}
.editor.invalid { border-color: var(--af-red); }
.editor.invalid:focus { border-color: var(--af-red); box-shadow: 0 0 0 3px var(--af-red-soft); }
.editor-foot {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  margin-top: 6px;
  font-size: 12px;
}
.editor-error { color: var(--af-red); }
.editor-hint { color: var(--af-faint); }
</style>
