<script setup lang="ts">
import { Tooltip } from "@arco-design/web-vue";
import { useRouter } from "vue-router";

const props = defineProps<{ term: string; description: string; target: string }>();
const router = useRouter();

function openHelp() {
  void router.push({ path: "/help", hash: `#${props.target}` });
}
</script>

<template>
  <Tooltip :content="description">
    <button class="term-help" type="button" :aria-label="`${term} 帮助`" @click="openHelp">?</button>
  </Tooltip>
</template>
