<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  Button,
  Divider,
  Layout,
  LayoutContent,
  LayoutHeader,
  LayoutSider,
  Menu,
  MenuItem,
  Space,
  Tag,
  Tooltip,
} from "@arco-design/web-vue";
import { useDashboardContext } from "../dashboard-context";
import { useSession } from "../session";

defineProps<{ theme: "dark" | "light" }>();
const emit = defineEmits<{ lock: []; toggleTheme: [] }>();
const route = useRoute();
const router = useRouter();
const session = useSession();
const context = useDashboardContext();
const collapsed = ref(false);

const primaryItems = [
  { path: "/overview", label: "概览", icon: "◈" },
  { path: "/services", label: "内网服务", icon: "⌁" },
  { path: "/nodes", label: "节点", icon: "◎" },
];
const secondaryItems = [
  { path: "/settings", label: "设置（高级）", icon: "⚙" },
];
const selectedPath = computed(() => route.path === "/" ? "/overview" : route.path);
const currentTitle = computed(() => String(route.meta.title || "Dashboard"));
const streamTone = computed(() => context.streamState.value === "connected" ? "green" : context.streamState.value === "offline" ? "gray" : "orange");
const streamLabel = computed(() => context.streamState.value === "connected" ? "实时连接" : context.streamState.value === "offline" ? "未连接" : "正在重连");

function navigate(path: string) {
  void router.push(path);
}

function clearError() {
  context.error.value = "";
}
</script>

<template>
  <Layout class="app-shell">
    <LayoutSider v-model:collapsed="collapsed" collapsible :width="236" :collapsed-width="72" breakpoint="xl" class="app-sider">
      <div class="brand-lockup">
        <div class="brand-mark small">AF</div>
        <div v-if="!collapsed">
          <strong>AsterFerry</strong>
          <span>Private network relay</span>
        </div>
      </div>
      <Menu :selected-keys="[selectedPath]" class="app-menu">
        <MenuItem v-for="item in primaryItems" :key="item.path" @click="navigate(item.path)">
          <template #icon><span class="menu-glyph">{{ item.icon }}</span></template>
          {{ item.label }}
        </MenuItem>
        <Divider v-if="!collapsed" class="menu-divider" />
        <MenuItem v-for="item in secondaryItems" :key="item.path" @click="navigate(item.path)">
          <template #icon><span class="menu-glyph">{{ item.icon }}</span></template>
          {{ item.label }}
        </MenuItem>
      </Menu>
      <div class="sider-bottom">
        <Menu :selected-keys="[selectedPath]" class="app-menu">
          <MenuItem key="/help" @click="navigate('/help')">
            <template #icon><span class="menu-glyph">?</span></template>
            帮助
          </MenuItem>
        </Menu>
      </div>
    </LayoutSider>
    <Layout>
      <LayoutHeader class="app-header">
        <div>
          <p class="eyebrow">ASTERFERRY / OPERATIONS</p>
          <h1>{{ currentTitle }}</h1>
        </div>
        <Space size="medium">
          <Tooltip content="事件流连接状态">
            <Tag :color="streamTone" bordered>{{ streamLabel }}</Tag>
          </Tooltip>
          <Button type="text" @click="emit('toggleTheme')">{{ theme === "dark" ? "浅色" : "深色" }}</Button>
          <Button type="text" @click="navigate('/help')">文档</Button>
          <Button type="outline" @click="session.lock(); emit('lock')">锁定</Button>
        </Space>
      </LayoutHeader>
      <LayoutContent class="app-content">
        <div v-if="context.error" class="global-error"><span>{{ context.error }}</span><Button type="text" @click="clearError">关闭</Button></div>
        <RouterView />
      </LayoutContent>
    </Layout>
  </Layout>
</template>
