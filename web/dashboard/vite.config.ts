import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const managementTarget = process.env.ASTERFERRY_DASHBOARD_TARGET || "http://127.0.0.1:9090";
const dashboardOutput = process.env.ASTERFERRY_DASHBOARD_OUT || "../../internal/dashboard/dist";

export default defineConfig({
  base: "/dashboard/",
  plugins: [react()],
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
});
