import { ref, watch } from "vue";

export type Theme = "light" | "dark";

const STORAGE_KEY = "asterferry.dashboard.theme";

function initialTheme(): Theme {
  const stored = window.localStorage.getItem(STORAGE_KEY);
  if (stored === "light" || stored === "dark") return stored;
  if (typeof window.matchMedia === "function" && window.matchMedia("(prefers-color-scheme: dark)").matches) return "dark";
  return "light";
}

// 模块级单例：主题全局唯一，立即应用以避免首屏闪烁。
const theme = ref<Theme>(initialTheme());

watch(
  theme,
  (value) => {
    document.documentElement.dataset.theme = value;
    window.localStorage.setItem(STORAGE_KEY, value);
  },
  { immediate: true },
);

export function useTheme() {
  function toggle() {
    theme.value = theme.value === "dark" ? "light" : "dark";
  }
  return { theme, toggle };
}
