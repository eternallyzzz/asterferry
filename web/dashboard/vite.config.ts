import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

const managementTarget = process.env.ASTERFERRY_DASHBOARD_TARGET || "http://127.0.0.1:9090";
const dashboardOutput = process.env.ASTERFERRY_DASHBOARD_OUT || "../../internal/dashboard/dist";

export default defineConfig({
  base: "/dashboard/",
  plugins: [vue()],
  server: {
    host: "127.0.0.1",
    proxy: {
      "/v1": managementTarget,
    },
  },
  build: {
    outDir: dashboardOutput,
    emptyOutDir: true,
    sourcemap: false,
  },
  test: {
    environment: "happy-dom",
  },
});
