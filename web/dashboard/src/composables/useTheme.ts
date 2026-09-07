import { ref, watch } from "vue";

export type Theme = "light" | "dark";

const STORAGE_KEY = "asterferry.dashboard.theme";

function initialTheme(): Theme {
  const stored = window.localStorage.getItem(STORAGE_KEY);
  if (stored === "light" || stored === "dark") return stored;
  if (typeof window.matchMedia === "function" && window.matchMedia("(prefers-color-scheme: dark)").matches) return "dark";
  return "light";
}

// Module singleton: the theme is global and applied immediately to avoid a
// first-paint flash.
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
