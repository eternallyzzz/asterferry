import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const managementTarget = process.env.ASTERFERRY_DASHBOARD_TARGET || "http://127.0.0.1:9090";

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
    outDir: "../../internal/dashboard/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
